// Package publish files a reviewed report on GitHub via the gh CLI,
// persisting state before every network step so a crash or timeout
// never produces a silent duplicate.
package publish

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
)

type Draft struct {
	ReportID   string // set once when Review opens; Submit with a known ID resumes instead of re-creating
	Mode       Mode
	Repo       string
	Title      string
	Body       string
	IssueRef   string
	Attachment []byte
	Queue      string
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
	Reviewed     Stage = "Reviewed"
	GistPending  Stage = "GistPending"
	GistCreated  Stage = "GistCreated"
	IssuePending Stage = "IssuePending"
	Published    Stage = "Published"
	Uncertain    Stage = "Uncertain"
)

// Saved is the on-disk state of one draft.
type Saved struct {
	ReportID  string
	Stage     Stage
	Draft     Draft
	GistURL   string
	IssueURL  string
	StartedAt time.Time
}

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

func Submit(ctx context.Context, gh Exec, stateDir string, d Draft) (Result, error) {
	if !slices.Contains(Repos, d.Repo) {
		return Result{}, fmt.Errorf("refusing to publish to %q: not an image repo that accepts reports", d.Repo)
	}
	if d.Mode == Confirm {
		n, err := ParseIssueRef(d.Repo, d.IssueRef)
		if err != nil {
			return Result{}, err
		}
		d.IssueRef = strconv.Itoa(n) // stored normalized: run and lookup use it verbatim
	}
	if _, _, err := gh(ctx, nil, "auth", "status"); err != nil {
		return Result{}, ErrNotSignedIn
	}
	if d.ReportID == "" {
		d.ReportID = NewReportID()
	}
	// Same report submitted again (retry after a failure): continue it, never start over.
	if s, err := load(filepath.Join(stateDir, d.ReportID, "state.json")); err == nil {
		if s.Stage == Published {
			return Result{IssueURL: s.IssueURL, GistURL: s.GistURL}, nil
		}
		return Resume(ctx, gh, stateDir, d.ReportID)
	}
	s := &Saved{ReportID: d.ReportID, Stage: Reviewed, Draft: d, StartedAt: time.Now().UTC()}
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
	if _, _, err := gh(ctx, nil, "auth", "status"); err != nil {
		return Result{}, ErrNotSignedIn
	}
	if s.Stage == Uncertain || s.Stage == GistPending || s.Stage == IssuePending {
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
	for _, f := range files {
		s, err := load(f)
		if err != nil {
			return nil, err
		}
		if s.Stage != Published {
			out = append(out, *s)
		}
	}
	return out, nil
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
	s.Stage = Reviewed
	if s.Draft.Attachment != nil && s.GistURL == "" {
		url, err := find("gists?per_page=100"+since,
			fmt.Sprintf(`.[] | select(.description == "bluefin-report %s") | .html_url`, s.ReportID))
		if err != nil {
			return err
		}
		if url == "" {
			return nil
		}
		s.GistURL = url
	}
	s.Stage = GistCreated
	marker := fmt.Sprintf(`.[] | select(.body // "" | contains("bluefin-report-id: %s")) | .html_url`, s.ReportID)
	var path string
	if s.Draft.Mode == Confirm {
		path = fmt.Sprintf("repos/%s/issues/%s/comments?per_page=100%s", s.Draft.Repo, s.Draft.IssueRef, since)
	} else {
		// creator=@me is not documented for this endpoint; use the real login.
		out, _, err := gh(ctx, nil, "api", "user", "--jq", ".login")
		if err != nil {
			return err
		}
		path = fmt.Sprintf("repos/%s/issues?creator=%s&state=all&per_page=100%s", s.Draft.Repo, strings.TrimSpace(string(out)), since)
	}
	url, err := find(path, marker)
	if err != nil {
		return err
	}
	if url != "" {
		s.IssueURL, s.Stage = url, Published
	}
	return nil
}

func run(ctx context.Context, gh Exec, dir string, s *Saved) (Result, error) {
	d := s.Draft
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
	if s.Stage == Reviewed {
		if d.Attachment != nil {
			url, err := step(GistPending, GistCreated, d.Attachment, "gist", "create",
				"--desc", "bluefin-report "+s.ReportID, "--filename", "systemd-report.json", "-")
			if err != nil {
				return Result{}, err
			}
			s.GistURL = url
		}
		s.Stage = GistCreated
		if err := save(dir, s); err != nil {
			return Result{}, err
		}
	}
	if s.Stage == GistCreated {
		body := d.Body
		if s.GistURL != "" {
			body += "\n\nReport data: " + s.GistURL
		}
		body += "\n\n<!-- bluefin-report-id: " + s.ReportID + " -->"
		if d.Queue != "" {
			body += "\n<!-- hive-queue: " + d.Queue + " -->"
		}
		args := []string{"issue", "create", "--repo", d.Repo, "--title", d.Title, "--body-file", "-"}
		if d.Mode == Confirm {
			args = []string{"issue", "comment", "--repo", d.Repo, "--body-file", "-", "--", d.IssueRef}
		}
		url, err := step(IssuePending, Published, []byte(body), args...)
		if err != nil {
			return Result{}, err
		}
		s.IssueURL = url
		if err := save(dir, s); err != nil {
			return Result{}, err
		}
	}
	return Result{IssueURL: s.IssueURL, GistURL: s.GistURL}, nil
}

func save(dir string, s *Saved) error {
	if s.Stage == Published {
		// Once on GitHub, the local copy of the reviewed payload has no use; keep only id and URLs.
		s.Draft.Attachment, s.Draft.Body = nil, ""
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
