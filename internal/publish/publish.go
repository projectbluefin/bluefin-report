// Package publish files a reviewed report on GitHub via the gh CLI,
// persisting state before every network step so a crash or timeout
// never produces a silent duplicate.
package publish

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	ShotsPending   Stage = "ShotsPending"   // NewIssue, artifacts refused: report done; screenshots comment not yet confirmed
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

// shotsArtifactName names the Markdown artifact that holds the uploaded screenshots.
func shotsArtifactName(d Draft) string {
	if d.Mode == Confirm || d.Mode == Update {
		return "screenshots-" + d.ReportID + ".md"
	}
	return "screenshots.md"
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
	// ShotsArtifact is set once the screenshots artifact exists; otherwise they go in a comment.
	ShotsArtifact bool `json:",omitempty"`
	// Digest covers the reviewed draft (screenshot bytes included); Resume refuses a draft edited on disk.
	// Empty for drafts saved before it existed, which are trusted.
	Digest string `json:",omitempty"`
}

// ErrDraftChanged means a saved draft no longer matches what was reviewed, so Resume will never publish it.
var ErrDraftChanged = errors.New("it was not published. Discard it from Drafts and start a new report")

var ErrNotSignedIn = errors.New("gh is not signed in; run `gh auth login`")

// Repos are the image repos that accept reports. A machine reports only to the repo of the image it runs.
var Repos = []string{"projectbluefin/dakota", "projectbluefin/utah"}

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
