// Package publish files a reviewed report on GitHub via the gh CLI,
// persisting state before every network step so a crash or timeout
// never produces a silent duplicate.
package publish

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Mode int

const (
	NewIssue Mode = iota
	Confirm
	Update
)

// Draft is one report. An Unfinished draft keeps the description as typed in Body and the steps to reproduce in Steps.
type Draft struct {
	ReportID   string // set once when Review opens; Submit with a known ID resumes instead of re-creating
	Mode       Mode
	Repo       string
	Title      string
	Body       string
	Steps      string `json:",omitempty"`
	IssueRef   string
	Attachment []byte
	Queue      string
	Area       string // NewIssue only: "" or desktop, flatpak, gaming, hardware, installer, dx
	// Screenshots are re-encoded (metadata stripped, content not scrubbed). Submit writes them into the draft dir and stores those paths.
	Screenshots []string
}

// Screenshot limits, checked before any network call.
const (
	MaxScreenshots     = 10
	MaxScreenshotBytes = 10 << 20
)

// ScreenshotExts are the attachable file types: those stdlib can decode and re-encode.
var ScreenshotExts = []string{".png", ".jpg", ".jpeg", ".gif"}

func checkScreenshots(paths []string) error {
	if len(paths) > MaxScreenshots {
		return fmt.Errorf("too many screenshots: %d (GitHub uploads are limited here to %d per report)", len(paths), MaxScreenshots)
	}
	for _, p := range paths {
		if !slices.Contains(ScreenshotExts, strings.ToLower(filepath.Ext(p))) {
			return fmt.Errorf("%s: not a supported screenshot type (%s)", filepath.Base(p), strings.Join(ScreenshotExts, ", "))
		}
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if fi.Size() > MaxScreenshotBytes {
			return fmt.Errorf("%s is %d MiB: each screenshot must be at most 10 MiB to upload", filepath.Base(p), fi.Size()>>20)
		}
	}
	return nil
}

// screenshotData prepares exactly the bytes and filename that will be uploaded.
func screenshotData(p string) ([]byte, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, "", err
	}
	img, _, err := image.Decode(f) // first frame only for GIF
	f.Close()
	if err != nil {
		return nil, "", fmt.Errorf("%s: not a readable image: %w", filepath.Base(p), err)
	}
	var b bytes.Buffer
	if ext := strings.ToLower(filepath.Ext(p)); ext == ".jpg" || ext == ".jpeg" {
		err = jpeg.Encode(&b, img, &jpeg.Options{Quality: 92})
	} else {
		p = strings.TrimSuffix(p, filepath.Ext(p)) + ".png"
		err = png.Encode(&b, img)
	}
	return b.Bytes(), filepath.Base(p), err
}

func screenshotDestination(dir, name string, i int, previous []string) string {
	dst := filepath.Join(dir, name)
	for n := i; slices.Contains(previous, dst); n++ {
		dst = filepath.Join(dir, fmt.Sprintf("%d-%s", n, name))
	}
	return dst
}

// copyScreenshots re-encodes the files into dir (0600) so no EXIF/text metadata survives
// and a resume survives the originals moving. Undecodable files are rejected.
// Copies never overwrite a path in previous.
func copyScreenshots(dir string, paths, previous []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var out []string
	for i, p := range paths {
		data, name, err := screenshotData(p)
		if err != nil {
			return nil, err
		}
		dst := screenshotDestination(dir, name, i, append(slices.Clip(previous), out...))
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, err
		}
		out = append(out, dst)
	}
	return out, nil
}

// sameScreenshots compares the ordered upload names and bytes, not original paths:
// saved drafts point at metadata-stripped copies, while Submit receives the originals.
func sameScreenshots(saved, reviewed []string) (bool, error) {
	if len(saved) != len(reviewed) {
		return false, nil
	}
	for i, p := range reviewed {
		want, err := os.ReadFile(saved[i])
		if err != nil {
			return false, err
		}
		if p == saved[i] {
			continue // already the draft copy; do not lossy-encode a JPEG twice
		}
		got, name, err := screenshotData(p)
		if err != nil {
			return false, err
		}
		dst := screenshotDestination(filepath.Dir(saved[i]), name, i, saved[:i])
		if dst != saved[i] || !bytes.Equal(want, got) {
			return false, nil
		}
	}
	return true, nil
}

// attachArgs returns gh --attach flags for every screenshot.
func attachArgs(paths []string) []string {
	var a []string
	for _, p := range paths {
		a = append(a, "--attach", p)
	}
	return a
}

// NewReportID returns a random id that tags every write of one report.
func NewReportID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type Result struct{ IssueURL, GistURL string }

type Exec func(ctx context.Context, stdin []byte, args ...string) (stdout, stderr []byte, err error)

type Stage string

const (
	Reviewed       Stage = "Reviewed"
	IssuePending   Stage = "IssuePending"
	IssueCreated   Stage = "IssueCreated" // issue exists; kind/area labels not yet ensured
	LabelsDone     Stage = "LabelsDone"   // labels ensured; report not yet attached
	GistPending    Stage = "GistPending"  // artifacts refused: fallback secret gist not yet created
	GistCreated    Stage = "GistCreated"  // fallback gist exists; its link comment not yet posted
	Published      Stage = "Published"
	ShotsPending   Stage = "ShotsPending"   // NewIssue: report done; screenshots comment not yet confirmed
	CommentPending Stage = "CommentPending" // Confirm/Update: report attached; comment not yet confirmed
	Uncertain      Stage = "Uncertain"
	Unfinished     Stage = "Unfinished" // saved while writing; never published until the user reviews it again
)

// artifactName gives each addition to an existing issue its own report artifact.
func artifactName(d Draft) string {
	if d.Mode == Confirm || d.Mode == Update {
		return "systemd-report-" + d.ReportID + ".json"
	}
	return "systemd-report.json"
}

// Saved is the on-disk state of one draft.
type Saved struct {
	ReportID  string
	Account   string // gh login authenticated before the first write; required to resume an uncertain write
	Stage     Stage
	Draft     Draft
	GistURL   string
	IssueURL  string
	StartedAt time.Time
	// Digest covers the reviewed draft (screenshot bytes included); Resume refuses a draft edited on disk.
	// Empty for drafts saved before it existed, which are trusted.
	Digest string `json:",omitempty"`
}

// ErrDraftChanged means a saved draft no longer matches what was reviewed, so Resume will never publish it.
var ErrDraftChanged = errors.New("it was not published. Discard it from Drafts and start a new report")

var ErrNotSignedIn = errors.New("gh is not signed in; run `gh auth login`")

// Repos are the image repos that accept reports. A machine reports only to the repo of the image it runs.
var Repos = []string{"projectbluefin/dakota", "projectbluefin/utah"}

func RunGH(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

type StreamExec func(ctx context.Context, stdin []byte, onStderr func(line string), args ...string) error

// RunGHStream runs gh, feeding every stdout/stderr line to onLine as it arrives.
func RunGHStream(ctx context.Context, stdin []byte, onLine func(string), args ...string) error {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	r, w := io.Pipe()
	cmd.Stdout, cmd.Stderr = w, w
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			onLine(sc.Text())
		}
		io.Copy(io.Discard, r)
		close(done)
	}()
	err := cmd.Run()
	w.Close()
	<-done
	return err
}

var (
	codeRe = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4}\b`)
	urlRe  = regexp.MustCompile(`https://\S+`)
)

// SignIn runs gh's device-code web login and reports the one-time code and URL once.
// Non-TTY gh prints "! First copy your one-time code: XXXX-XXXX" then
// "Open this URL to continue in your web browser: https://github.com/login/device".
func SignIn(ctx context.Context, gh StreamExec, onCode func(code, url string)) error {
	var mu sync.Mutex
	var code string
	sent := false
	send := func(url string) {
		if !sent && code != "" {
			sent = true
			onCode(code, url)
		}
	}
	err := gh(ctx, nil, func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if code == "" && strings.Contains(strings.ToLower(line), "code") {
			code = codeRe.FindString(line)
		}
		if u := urlRe.FindString(line); u != "" {
			send(u)
		}
	}, "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web", "--skip-ssh-key")
	mu.Lock()
	send("https://github.com/login/device")
	mu.Unlock()
	return err
}

// ErrUnfinished means a draft was saved before review, so it must be reopened and reviewed, not resumed.
var ErrUnfinished = errors.New("this report was saved unfinished. Open it from Drafts to keep writing")

// SaveUnfinished stores d as an Unfinished draft under d.ReportID, replacing an earlier save of
// the same report, and returns the saved screenshot copies. It never touches GitHub and refuses
// to overwrite a report that was reviewed.
func SaveUnfinished(stateDir string, d Draft) ([]string, error) {
	if d.ReportID == "" || strings.ContainsAny(d.ReportID, `/\.`) {
		return nil, fmt.Errorf("bad report id %q", d.ReportID)
	}
	dir := filepath.Join(stateDir, d.ReportID)
	s, err := load(filepath.Join(dir, "state.json"))
	switch {
	case err == nil && s.Stage != Unfinished:
		return nil, fmt.Errorf("report %s is already being published", d.ReportID)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	shotDir := filepath.Join(dir, "screenshots")
	var keep, fresh []string
	for _, p := range d.Screenshots {
		switch {
		case filepath.Dir(p) != shotDir:
			fresh = append(fresh, p)
		case fileExists(p): // a missing copy means the draft was discarded meanwhile; drop it
			keep = append(keep, p) // already a draft copy; do not lossy-encode it twice
		}
	}
	if err := checkScreenshots(fresh); err != nil {
		return nil, err
	}
	copies, err := copyScreenshots(shotDir, fresh, keep)
	if err != nil {
		return nil, err
	}
	d.Screenshots = append(keep, copies...)
	d.Attachment = nil
	started := time.Now().UTC()
	if s != nil {
		started = s.StartedAt
	}
	if err := save(stateDir, &Saved{ReportID: d.ReportID, Stage: Unfinished, Draft: d, StartedAt: started}); err != nil {
		return nil, err
	}
	return d.Screenshots, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Discard deletes a local draft; it never touches GitHub.
func Discard(stateDir, reportID string) error {
	if reportID == "" || strings.ContainsAny(reportID, `/\.`) {
		return fmt.Errorf("bad report id %q", reportID)
	}
	return os.RemoveAll(filepath.Join(stateDir, reportID))
}

// Route returns the repo for the running image's IMAGE_ID, or "" when the image has no report repo.
func Route(imageID string) string {
	for _, r := range Repos {
		if name := strings.TrimPrefix(r, "projectbluefin/"); imageID == name || strings.HasPrefix(imageID, name+"-") {
			return r
		}
	}
	return ""
}

// ParseIssueRef accepts only "N", "#N" or https://github.com/<repo>/issues/N[#...].
func ParseIssueRef(repo, ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if p := "https://github.com/" + repo + "/issues/"; len(ref) > len(p) && strings.EqualFold(ref[:len(p)], p) {
		ref, _, _ = strings.Cut(ref[len(p):], "#")
	} else {
		ref = strings.TrimPrefix(ref, "#")
	}
	n, err := strconv.Atoi(ref)
	if err != nil || n <= 0 || ref[0] < '1' || ref[0] > '9' {
		return 0, fmt.Errorf("not an issue in %s: %q", repo, ref)
	}
	return n, nil
}

// issueBody is the exact NewIssue payload, including all publisher-added text.
func issueBody(d Draft) string {
	body := d.Body
	if d.Attachment != nil {
		body += "\n\nFull report: `" + artifactName(d) + "`, attached to this issue (or linked in a comment below)"
	}
	body += "\n\n<!-- bluefin-report-id: " + d.ReportID + " -->"
	if d.Queue != "" {
		body += "\n<!-- hive-queue: " + d.Queue + " -->"
	}
	return body
}

func validateDraft(d Draft) error {
	switch d.Mode {
	case NewIssue:
		if n := len(issueBody(d)); n > 60*1024 {
			return fmt.Errorf("issue body is %d bytes; the limit is %d bytes including report metadata. Return to Review and shorten the report by at least %d bytes; nothing was published", n, 60*1024, n-60*1024)
		}
	case Confirm, Update:
		if _, err := ParseIssueRef(d.Repo, d.IssueRef); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown publish mode %d", d.Mode)
	}
	return nil
}

func authenticatedAccount(ctx context.Context, gh Exec) (string, error) {
	out, _, err := gh(ctx, nil, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	account := strings.TrimSpace(string(out))
	if account == "" {
		return "", ErrNotSignedIn
	}
	return account, nil
}

func Submit(ctx context.Context, gh Exec, stateDir string, d Draft) (Result, error) {
	if !slices.Contains(Repos, d.Repo) {
		return Result{}, fmt.Errorf("refusing to publish to %q: not an image repo that accepts reports", d.Repo)
	}
	if d.Mode == Confirm || d.Mode == Update {
		n, err := ParseIssueRef(d.Repo, d.IssueRef)
		if err != nil {
			return Result{}, err
		}
		d.IssueRef = strconv.Itoa(n) // stored normalized: run and lookup use it verbatim
	}
	if d.ReportID == "" {
		d.ReportID = NewReportID()
	}
	s, err := load(filepath.Join(stateDir, d.ReportID, "state.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	if err == nil && s.Stage == Published {
		return Result{IssueURL: s.IssueURL, GistURL: s.GistURL}, nil
	}
	if err := validateDraft(d); err != nil {
		return Result{}, err
	}
	if err := checkScreenshots(d.Screenshots); err != nil {
		return Result{}, err
	}
	// Same report submitted again: only resume the payload that was reviewed.
	if s != nil {
		saved := s.Draft
		same := saved.ReportID == d.ReportID && saved.Mode == d.Mode && saved.Repo == d.Repo &&
			saved.Title == d.Title && saved.Body == d.Body && saved.IssueRef == d.IssueRef &&
			saved.Queue == d.Queue && saved.Area == d.Area &&
			(saved.Attachment == nil) == (d.Attachment == nil) && bytes.Equal(saved.Attachment, d.Attachment)
		if same {
			same, err = sameScreenshots(saved.Screenshots, d.Screenshots)
			if err != nil {
				return Result{}, err
			}
		}
		if !same {
			return Result{}, fmt.Errorf("report %s differs from its saved draft; the changed report was not published. Resume the unchanged saved version from Drafts, or start a new report for these edits", d.ReportID)
		}
		return Resume(ctx, gh, stateDir, d.ReportID)
	}
	if _, _, err := gh(ctx, nil, "auth", "status"); err != nil {
		return Result{}, ErrNotSignedIn
	}
	// Persist the authenticated identity before the first write so an uncertain write can later
	// be resolved only under the account that started it; a switch to another account must not be
	// able to look the write up as "not found" and resume it as a duplicate.
	account, err := authenticatedAccount(ctx, gh)
	if err != nil {
		return Result{}, err
	}
	if d.Mode == Update { // an update speaks for the original reporter, so only they may post it
		out, errb, err := gh(ctx, nil, "api", "repos/"+d.Repo+"/issues/"+d.IssueRef, "--jq", ".user.login")
		if err != nil {
			return Result{}, fmt.Errorf("could not read who opened issue #%s: %w: %s", d.IssueRef, err, strings.TrimSpace(string(errb)))
		}
		if author := strings.TrimSpace(string(out)); !strings.EqualFold(author, account) {
			return Result{}, fmt.Errorf("issue #%s was opened by %s, but %s is signed in. Only the original reporter can add an update; use Confirm Existing Issue instead", d.IssueRef, author, account)
		}
	}
	shots, err := copyScreenshots(filepath.Join(stateDir, d.ReportID, "screenshots"), d.Screenshots, nil)
	if err != nil {
		return Result{}, err
	}
	d.Screenshots = shots
	s = &Saved{ReportID: d.ReportID, Account: account, Stage: Reviewed, Draft: d, StartedAt: time.Now().UTC()}
	if s.Digest, err = digest(d); err != nil {
		return Result{}, err
	}
	if err := save(stateDir, s); err != nil {
		return Result{}, err
	}
	return run(ctx, gh, stateDir, s)
}

// Resume continues an unfinished draft, resolving Uncertain by lookup first.
func Resume(ctx context.Context, gh Exec, stateDir, reportID string) (Result, error) {
	s, err := load(filepath.Join(stateDir, reportID, "state.json"))
	if err != nil {
		return Result{}, err
	}
	if s.Stage == Unfinished {
		return Result{}, ErrUnfinished
	}
	if !slices.Contains(Repos, s.Draft.Repo) { // drafts saved under older routing
		return Result{}, fmt.Errorf("refusing to publish to %q: not an image repo that accepts reports", s.Draft.Repo)
	}
	if s.Stage != Published && s.Digest != "" {
		got, err := digest(s.Draft)
		if err != nil {
			return Result{}, fmt.Errorf("report %s: saved draft is damaged (%v); %w", reportID, err, ErrDraftChanged)
		}
		if got != s.Digest {
			return Result{}, fmt.Errorf("report %s was changed on disk after review; %w", reportID, ErrDraftChanged)
		}
	}
	if s.Stage == Published {
		return Result{IssueURL: s.IssueURL, GistURL: s.GistURL}, nil
	}
	if err := validateDraft(s.Draft); err != nil {
		return Result{}, err
	}
	if _, _, err := gh(ctx, nil, "auth", "status"); err != nil {
		return Result{}, ErrNotSignedIn
	}
	// Interrupted writes, and drafts from older versions that hold no issue yet, are resolved by lookup.
	if s.Stage == Uncertain || s.Stage == GistPending || s.Stage != Reviewed && s.IssueURL == "" {
		// An uncertain write was started under a specific account. If gh is now signed in as someone
		// else, an account-scoped lookup would miss the original write and resume would duplicate it,
		// so stop with guidance instead of performing another remote write. Drafts saved before the
		// account was recorded (s.Account == "") cannot be bound, so fall back to the plain lookup.
		if s.Account != "" {
			got, err := authenticatedAccount(ctx, gh)
			if err != nil {
				return Result{}, err
			}
			if got != s.Account {
				return Result{}, fmt.Errorf("cannot resume report %s: it was started with gh account %q, but gh is now signed in as %q. Sign back into %q with \"gh auth login\", resume there, or discard this draft and start a new report", s.ReportID, s.Account, got, s.Account)
			}
		}
		if err := lookup(ctx, gh, s); err != nil {
			return Result{}, err
		}
		if err := save(stateDir, s); err != nil {
			return Result{}, err
		}
	}
	return run(ctx, gh, stateDir, s)
}

// Pending lists drafts that are not Published.
func Pending(stateDir string) ([]Saved, error) {
	files, err := filepath.Glob(filepath.Join(stateDir, "*", "state.json"))
	if err != nil {
		return nil, err
	}
	var out []Saved
	var errs []error
	for _, f := range files {
		s, err := load(f)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(filepath.Dir(f)), err))
			continue // one unreadable draft must not hide the others
		}
		if s.Stage != Published {
			out = append(out, *s)
		}
	}
	return out, errors.Join(errs...)
}

// digest hashes everything Review showed: the draft fields and each screenshot copy's name and bytes.
func digest(d Draft) (string, error) {
	shots := d.Screenshots
	d.Screenshots = nil
	b, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d:", len(b))
	h.Write(b)
	for _, p := range shots {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s:%d:", filepath.Base(p), len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// lookup decides from GitHub what an interrupted write actually did. It uses REST
// list endpoints (consistent) rather than search (lagging index), so "not found"
// really means not created and the stage before the write is retried.
func lookup(ctx context.Context, gh Exec, s *Saved) error {
	since := ""
	if !s.StartedAt.IsZero() {
		since = "&since=" + s.StartedAt.Add(-5*time.Minute).UTC().Format(time.RFC3339)
	}
	find := func(path, jq string) (string, error) {
		out, _, err := gh(ctx, nil, "api", "--paginate", path, "--jq", jq)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]), nil
	}
	if s.IssueURL != "" { // the issue exists; only the fallback gist is in doubt
		url, err := find("gists?per_page=100"+since,
			fmt.Sprintf(`.[] | select(.description == "bluefin-report %s") | .html_url`, s.ReportID))
		if err != nil {
			return err
		}
		s.GistURL, s.Stage = url, GistCreated
		if url == "" {
			s.Stage = GistPending
		}
		return nil
	}
	s.Stage = Reviewed
	marker := fmt.Sprintf(`.[] | select(.body // "" | contains("bluefin-report-id: %s")) | .html_url`, s.ReportID)
	var path string
	if s.Draft.Mode == Confirm || s.Draft.Mode == Update {
		path = fmt.Sprintf("repos/%s/issues/%s/comments?per_page=100%s", s.Draft.Repo, s.Draft.IssueRef, since)
	} else {
		// creator=@me is not documented for this endpoint; use the real login. Unbound legacy drafts look it up.
		creator := s.Account
		if creator == "" {
			var err error
			if creator, err = authenticatedAccount(ctx, gh); err != nil {
				return err
			}
		}
		path = fmt.Sprintf("repos/%s/issues?creator=%s&state=all&per_page=100%s", s.Draft.Repo, creator, since)
	}
	url, err := find(path, marker)
	if err != nil {
		return err
	}
	if url != "" {
		s.IssueURL, s.Stage = url, Published
		if s.Draft.Mode == NewIssue {
			s.Stage = IssueCreated
		}
	}
	return nil
}

func run(ctx context.Context, gh Exec, dir string, s *Saved) (Result, error) {
	d := s.Draft
	name := artifactName(d)
	if (d.Mode == Confirm || d.Mode == Update) && s.Stage == Reviewed { // existing issue: attach directly
		s.IssueURL, s.Stage = "https://github.com/"+d.Repo+"/issues/"+d.IssueRef, LabelsDone
	}
	step := func(pending, done Stage, stdin []byte, args ...string) (string, error) {
		s.Stage = pending
		if err := save(dir, s); err != nil {
			return "", err
		}
		out, errb, err := gh(ctx, stdin, args...)
		if err != nil {
			// Any failure may have happened after GitHub accepted the write (timeout, killed gh):
			// only a lookup can tell, so never assume nothing was created.
			s.Stage = Uncertain
			save(dir, s)
			return "", fmt.Errorf("gh %s: %w: %s", args[0], err, strings.TrimSpace(string(errb)))
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		s.Stage = done
		return lines[len(lines)-1], nil
	}
	fresh, labeled := false, false // labels known only when the issue was created in this run
	if s.Stage == Reviewed {
		body := issueBody(d)
		// REST create: labels/type are silently dropped for non-maintainers instead of failing the create.
		args := []string{"api", "--method", "POST", "repos/" + d.Repo + "/issues", "-f", "title=" + d.Title,
			"-F", "body=@-", "-f", "labels[]=kind/bug"}
		if d.Area != "" {
			args = append(args, "-f", "labels[]=area/"+d.Area)
		}
		args = append(args, "-f", "type=Bug", "--jq", `.html_url + " " + ([.labels[].name]|join(","))`)
		done := IssueCreated
		out, err := step(IssuePending, done, []byte(body), args...)
		if err != nil {
			return Result{}, err
		}
		url, labels, _ := strings.Cut(out, " ")
		s.IssueURL, fresh, labeled = url, true, hasLabels(d, strings.Split(labels, ","))
		if err := save(dir, s); err != nil {
			return Result{}, err
		}
	}
	r := Result{IssueURL: s.IssueURL, GistURL: s.GistURL}
	n := s.IssueURL[strings.LastIndex(s.IssueURL, "/")+1:]
	// Issue already exists from here on: failures are warnings, and the next Resume re-checks before writing.
	warn := func(what string, err error, errb []byte) (Result, error) {
		return r, fmt.Errorf("issue filed at %s, but %s failed (retry to finish): %w: %s", s.IssueURL, what, err, strings.TrimSpace(string(errb)))
	}
	final := Published
	if len(d.Screenshots) > 0 && d.Mode == NewIssue {
		final = ShotsPending
	}
	if d.Mode == Confirm || d.Mode == Update {
		final = CommentPending
	}
	freshShots := false // ShotsPending reached in this run: no comment can exist yet
	if s.Stage == IssueCreated {
		if !fresh {
			out, errb, err := gh(ctx, nil, "issue", "view", n, "-R", d.Repo, "--json", "labels", "--jq", "[.labels[].name]|join(\",\")")
			if err != nil {
				return warn("reading labels", err, errb)
			}
			labeled = hasLabels(d, strings.Split(strings.TrimSpace(string(out)), ","))
		}
		posted := false
		if !labeled && !fresh {
			login, errb, err := gh(ctx, nil, "api", "user", "--jq", ".login")
			if err != nil {
				return warn("reading user", err, errb)
			}
			out, errb, err := gh(ctx, nil, "api", "--paginate", fmt.Sprintf("repos/%s/issues/%s/comments?per_page=100", d.Repo, n),
				"--jq", fmt.Sprintf(`.[] | select(.user.login == %q and (.body | startswith("/kind bug"))) | .id`, strings.TrimSpace(string(login))))
			if err != nil {
				return warn("reading comments", err, errb)
			}
			posted = strings.TrimSpace(string(out)) != ""
		}
		if !labeled && !posted {
			// Prow applies /kind and /area for any commenter.
			cmd := "/kind bug"
			if d.Area != "" {
				cmd += "\n/area " + d.Area
			}
			if _, errb, err := gh(ctx, []byte(cmd), "issue", "comment", "--repo", d.Repo, "--body-file", "-", "--", n); err != nil {
				return warn("labeling", err, errb)
			}
		}
		s.Stage = LabelsDone
		if err := save(dir, s); err != nil {
			return r, err
		}
	}
	if s.Stage == LabelsDone {
		// The full report goes on the issue as a fenced-JSON artifact. Only when GitHub or an older gh
		// refuses artifacts outright (no permission, preview unavailable) does it fall back to a secret
		// gist linked in a comment; transient failures leave the draft pending for Resume.
		exists, refused := false, false
		if d.Attachment != nil && !fresh {
			out, errb, err := gh(ctx, nil, "issue", "artifact", "list", n, "-R", d.Repo, "--json", "name", "--jq", ".[].name")
			if err != nil && !artifactsRefused(errb) {
				return warn("reading artifacts", err, errb)
			}
			refused = err != nil
			exists = slices.Contains(strings.Split(strings.TrimSpace(string(out)), "\n"), name)
		}
		if d.Attachment != nil && !exists && !refused {
			doc := slices.Concat([]byte("```json\n"), d.Attachment, []byte("\n```\n"))
			if _, errb, err := gh(ctx, doc, "issue", "artifact", "create", n, "--body-file", "-", "--name", name, "-R", d.Repo); err != nil {
				if !artifactsRefused(errb) {
					return warn("attaching the report", err, errb)
				}
				refused = true
			}
		}
		s.Stage, freshShots = final, true
		if refused {
			s.Stage = GistPending
		}
		if err := save(dir, s); err != nil {
			return r, err
		}
	}
	freshGist := false
	if s.Stage == GistPending {
		url, err := step(GistPending, GistCreated, d.Attachment, "gist", "create",
			"--desc", "bluefin-report "+s.ReportID, "--filename", name, "-")
		if err != nil {
			return r, fmt.Errorf("issue filed at %s, but attaching the report failed (retry to finish): %w", s.IssueURL, err)
		}
		s.GistURL, r.GistURL, freshGist = url, url, true
		if err := save(dir, s); err != nil {
			return r, err
		}
	}
	if s.Stage == GistCreated {
		marker := "<!-- bluefin-report-id: " + s.ReportID + " -->"
		posted := false
		if !freshGist && d.Mode == NewIssue { // existing-issue modes link the gist in their one comment
			out, errb, err := gh(ctx, nil, "api", "--paginate", fmt.Sprintf("repos/%s/issues/%s/comments?per_page=100", d.Repo, n),
				"--jq", fmt.Sprintf(`.[] | select(.body // "" | contains(%q)) | .id`, marker))
			if err != nil {
				return warn("reading comments", err, errb)
			}
			posted = strings.TrimSpace(string(out)) != ""
		}
		if !posted && d.Mode == NewIssue {
			if _, errb, err := gh(ctx, []byte("Full report: "+s.GistURL+"\n"+marker), "issue", "comment", "--repo", d.Repo, "--body-file", "-", "--", n); err != nil {
				return warn("linking the report", err, errb)
			}
		}
		s.Stage, freshShots = final, true
		if err := save(dir, s); err != nil {
			return r, err
		}
	}
	if s.Stage == ShotsPending {
		marker := "<!-- bluefin-report-screenshots: " + s.ReportID + " -->"
		posted := false
		if !freshShots {
			out, errb, err := gh(ctx, nil, "api", "--paginate", fmt.Sprintf("repos/%s/issues/%s/comments?per_page=100", d.Repo, n),
				"--jq", fmt.Sprintf(`.[] | select(.body // "" | contains(%q)) | .id`, marker))
			if err != nil {
				return warn("reading comments", err, errb)
			}
			posted = strings.TrimSpace(string(out)) != ""
		}
		if !posted {
			args := append([]string{"issue", "comment", "--repo", d.Repo, "--body-file", "-"}, attachArgs(d.Screenshots)...)
			if _, errb, err := gh(ctx, []byte("Screenshots\n"+marker), append(args, "--", n)...); err != nil {
				return warn("uploading screenshots", err, errb)
			}
		}
		s.Stage = Published
		if err := save(dir, s); err != nil {
			return r, err
		}
	}
	if s.Stage == CommentPending {
		marker := "<!-- bluefin-report-id: " + s.ReportID + " -->"
		posted := false
		if !freshShots {
			out, errb, err := gh(ctx, nil, "api", "--paginate", fmt.Sprintf("repos/%s/issues/%s/comments?per_page=100", d.Repo, n),
				"--jq", fmt.Sprintf(`.[] | select(.body // "" | contains(%q)) | .id`, marker))
			if err != nil {
				return warn("reading comments", err, errb)
			}
			posted = strings.TrimSpace(string(out)) != ""
		}
		if !posted {
			body := d.Body
			switch {
			case s.GistURL != "":
				body += "\n\nFull report: " + s.GistURL
			case d.Attachment != nil:
				body += "\n\nFull report: `" + name + "`, attached to this issue"
			}
			kind := "confirm"
			if d.Mode == Update {
				kind = "update"
			}
			body += "\n\n" + marker + "\n<!-- bluefin-report-" + kind + " -->"
			args := append([]string{"issue", "comment", "--repo", d.Repo, "--body-file", "-"}, attachArgs(d.Screenshots)...)
			if _, errb, err := gh(ctx, []byte(body), append(args, "--", n)...); err != nil {
				return warn("commenting", err, errb)
			}
		}
		s.Stage = Published
		if err := save(dir, s); err != nil {
			return r, err
		}
	}
	return r, nil
}

// artifactsRefused reports a permanent refusal (permission, missing preview command), as opposed to
// a network failure worth retrying.
func artifactsRefused(stderr []byte) bool {
	s := string(stderr)
	for _, m := range []string{"HTTP 401", "HTTP 403", "HTTP 404", "HTTP 410", "HTTP 422", "unknown command", "Resource not accessible", "must have"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// hasLabels reports whether GitHub applied every label we asked for.
func hasLabels(d Draft, got []string) bool {
	return slices.Contains(got, "kind/bug") && (d.Area == "" || slices.Contains(got, "area/"+d.Area))
}

func save(dir string, s *Saved) error {
	if s.Stage == Published {
		// Once on GitHub, the local copy of the reviewed payload has no use; keep only id and URLs.
		s.Draft.Attachment, s.Draft.Body, s.Draft.Screenshots = nil, "", nil
		if err := os.RemoveAll(filepath.Join(dir, s.ReportID, "screenshots")); err != nil {
			return err
		}
	}
	d := filepath.Join(dir, s.ReportID)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(d, "state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(d, "state.json"))
}

func load(path string) (*Saved, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Saved
	return &s, json.Unmarshal(b, &s)
}
