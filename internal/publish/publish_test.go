package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fake struct {
	calls []string
	stdin []string
	fail  map[string]bool   // keyed by "gh sub" or "api gists|issues|comments|user"
	errs  map[string]string // stderr for a failing key; default "boom"
	out   map[string]string
}

const create = "api --method POST repos/projectbluefin/dakota/issues"

func (f *fake) exec(_ context.Context, in []byte, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	f.stdin = append(f.stdin, string(in))
	k := args[0] + " " + args[1]
	if args[1] == "artifact" {
		k = "artifact " + args[2]
	}
	if args[0] == "api" {
		p := args[1]
		if p == "--paginate" {
			p = args[2]
		}
		switch {
		case p == "--method":
			k = "issue create"
		case strings.Contains(p, "/comments"):
			k = "api comments"
		case strings.Contains(p, "/issues"):
			k = "api issues"
		default:
			k = "api " + strings.SplitN(p, "?", 2)[0]
		}
	}
	if f.fail[k] {
		if e, ok := f.errs[k]; ok {
			return nil, []byte(e), errors.New("exit 1")
		}
		return nil, []byte("boom"), errors.New("exit 1")
	}
	return []byte(f.out[k]), nil, nil
}

// in returns the stdin of the first call starting with prefix.
func (f *fake) in(prefix string) string {
	for i, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return f.stdin[i]
		}
	}
	return ""
}

func (f *fake) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
		if strings.Contains(c, "--label") {
			panic("--label used")
		}
	}
	return n
}

func newFake() *fake {
	return &fake{fail: map[string]bool{}, out: map[string]string{
		"gist create":   "https://gist.github.com/abc\n",
		"issue create":  "https://github.com/projectbluefin/dakota/issues/7 kind/bug,needs-triage\n",
		"issue comment": "https://github.com/projectbluefin/dakota/issues/3#c1\n",
		"api user":      "me\n",
	}}
}

var draft = Draft{Repo: "projectbluefin/dakota", Title: "t", Body: "b", Attachment: []byte("{}"), Queue: "3-human-queue"}

func TestRoute(t *testing.T) {
	for id, want := range map[string]string{
		"dakota": "projectbluefin/dakota", "dakota-gaming": "projectbluefin/dakota", "dakota-nvidia": "projectbluefin/dakota",
		"utah": "projectbluefin/utah", "utah-dx": "projectbluefin/utah",
		"bluefin": "", "bluefin-lts": "", "dakotax": "", "": "",
	} {
		if got := Route(id); got != want {
			t.Errorf("Route(%q)=%q, want %q", id, got, want)
		}
	}
	for _, repo := range []string{"evil/dakota", "projectbluefin/bluefin", "projectbluefin/bluefin-lts", "projectbluefin/common"} {
		f := newFake()
		if _, err := Submit(context.Background(), f.exec, t.TempDir(), Draft{Repo: repo}); err == nil || len(f.calls) != 0 {
			t.Errorf("%s not rejected before network", repo)
		}
	}
}

const artifact = "issue artifact create 7 --body-file - --name systemd-report.json -R projectbluefin/dakota"

func TestSuccess(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := draft
	d.ReportID = NewReportID()
	r, err := Submit(context.Background(), f.exec, dir, d)
	if err != nil || r.GistURL != "" || !strings.HasSuffix(r.IssueURL, "/issues/7") {
		t.Fatal(r, err)
	}
	if f.count("gist") != 0 || f.count(create) != 1 || f.count("issue artifact list") != 0 || f.count(artifact) != 1 {
		t.Fatal(f.calls)
	}
	if in := f.in(artifact); in != "```json\n{}\n```\n" || !strings.HasPrefix(in, "```json\n") || !strings.HasSuffix(in, "\n```\n") {
		t.Fatalf("artifact is not fenced JSON: %q", in)
	}
	if b := f.in(create); !strings.Contains(b, "Full report: `systemd-report.json`, attached to this issue (or linked in a comment below)") ||
		!strings.Contains(b, "<!-- bluefin-report-id: "+d.ReportID+" -->") || !strings.Contains(b, "<!-- hive-queue: 3-human-queue -->") || strings.Contains(b, "gist") {
		t.Fatalf("issue body: %q", b)
	}
	if p, _ := Pending(dir); len(p) != 0 {
		t.Fatal("not Published", p)
	}
	s, err := load(filepath.Join(dir, d.ReportID, "state.json"))
	if err != nil || s.Draft.Attachment != nil || s.Draft.Body != "" || s.IssueURL == "" {
		t.Fatalf("published draft still holds the payload or lost its URL: %+v %v", s, err)
	}
	if r2, err := Submit(context.Background(), f.exec, dir, d); err != nil || r2 != r || f.count(create) != 1 || f.count(artifact) != 1 {
		t.Fatal("re-submit of a published report must return the stored result", r2, err, f.calls)
	}
}

func TestSubmitRejectsChangedDraftOnRetry(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Draft)
	}{
		{"body", func(d *Draft) { d.Body = "freshly reviewed body" }},
		{"title", func(d *Draft) { d.Title = "freshly reviewed title" }},
		{"repo", func(d *Draft) { d.Repo = "projectbluefin/utah" }},
		{"mode", func(d *Draft) { d.Mode = Confirm }},
		{"issue reference", func(d *Draft) { d.IssueRef = "4" }},
		{"queue", func(d *Draft) { d.Queue = "another-queue" }},
		{"area", func(d *Draft) { d.Area = "gaming" }},
		{"removed area", func(d *Draft) { d.Area = "" }},
		{"attachment", func(d *Draft) { d.Attachment = []byte(`{"reviewed":true}`) }},
		{"removed attachment", func(d *Draft) { d.Attachment = nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, dir := newFake(), t.TempDir()
			f.fail["issue create"] = true
			d := draft
			d.ReportID, d.IssueRef, d.Area = NewReportID(), "3", "desktop"
			if _, err := Submit(context.Background(), f.exec, dir, d); err == nil {
				t.Fatal("want first issue write to fail")
			}
			path := filepath.Join(dir, d.ReportID, "state.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			calls := len(f.calls)
			remoteCalls := f.count("api") + f.count("gist") + f.count("issue")
			f.fail["issue create"] = false
			changed := d
			tt.change(&changed)
			if _, err := Submit(context.Background(), f.exec, dir, changed); err == nil {
				t.Fatal("want draft mismatch error")
			}
			if f.count("api")+f.count("gist")+f.count("issue") != remoteCalls {
				t.Fatal("changed retry must not look up or publish the saved payload", f.calls[calls:])
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("changed retry modified saved state", err)
			}
			// The unchanged consented draft can still resolve the uncertain issue
			// write and attach its report through the current artifact workflow.
			r, err := Submit(context.Background(), f.exec, dir, d)
			if err != nil || r.IssueURL == "" || f.count("gist create") != 0 || f.count(create) != 2 || f.count(artifact) != 1 {
				t.Fatal("unchanged retry failed", r, err, f.calls)
			}
		})
	}
}

func TestSubmitRejectsChangedScreenshotsOnRetry(t *testing.T) {
	for _, mode := range []Mode{NewIssue, Confirm} {
		for _, name := range []string{"added", "removed", "reordered", "renamed", "replaced", "edited in place"} {
			t.Run(fmt.Sprintf("%d/%s", mode, name), func(t *testing.T) {
				f, dir := newFake(), t.TempDir()
				d := draft
				d.ReportID, d.Mode, d.IssueRef = NewReportID(), mode, "3"
				d.Screenshots = shots(t, 2, ".png", 10)
				f.fail["artifact create"] = true
				if _, err := Submit(context.Background(), f.exec, dir, d); err == nil {
					t.Fatal("want artifact failure after the issue is known")
				}
				path := filepath.Join(dir, d.ReportID, "state.json")
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				changed := d
				changed.Screenshots = slices.Clone(d.Screenshots)
				switch name {
				case "added":
					changed.Screenshots = append(changed.Screenshots, shots(t, 1, ".png", 10)...)
				case "removed":
					changed.Screenshots = changed.Screenshots[:1]
				case "reordered":
					slices.Reverse(changed.Screenshots)
				case "renamed":
					data, err := os.ReadFile(changed.Screenshots[0])
					if err != nil {
						t.Fatal(err)
					}
					changed.Screenshots[0] = filepath.Join(t.TempDir(), "renamed.png")
					if err := os.WriteFile(changed.Screenshots[0], data, 0o600); err != nil {
						t.Fatal(err)
					}
				case "replaced", "edited in place":
					if name == "replaced" {
						changed.Screenshots[0] = filepath.Join(t.TempDir(), "s.png")
					}
					var b bytes.Buffer
					img := image.NewGray(image.Rect(0, 0, 2, 2))
					if err := png.Encode(&b, img); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(changed.Screenshots[0], b.Bytes(), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				calls := len(f.calls)
				if _, err := Submit(context.Background(), f.exec, dir, changed); err == nil {
					t.Fatal("changed screenshots must not publish the saved payload")
				}
				if len(f.calls) != calls {
					t.Fatal("changed retry caused remote work", f.calls[calls:])
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("changed retry modified saved state", err)
				}
				// Explicit Resume still uses the original, private draft copies.
				f.fail["artifact create"] = false
				if _, err := Resume(context.Background(), f.exec, dir, d.ReportID); err != nil {
					t.Fatal("explicit resume failed", err, f.calls)
				}
				comments := 0
				if mode == Confirm {
					comments = 1
				}
				shotUploads := 0
				for _, c := range f.calls {
					if strings.HasPrefix(c, "issue artifact create") && strings.Contains(c, "--name screenshots") {
						shotUploads++
					}
				}
				if f.count("issue artifact create") != 3 || shotUploads != 1 || f.count("issue comment") != comments ||
					slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "issue comment") && strings.Contains(c, "--attach") }) {
					t.Fatal("resume duplicated or lost report uploads", f.calls)
				}
				if p, err := Pending(dir); err != nil || len(p) != 0 {
					t.Fatal("resume did not publish", p, err)
				}
			})
		}
	}
}

func TestSubmitRetriesUnchangedScreenshots(t *testing.T) {
	for _, ext := range []string{".png", ".jpg", ".gif"} {
		for _, savedPaths := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/saved=%t", ext, savedPaths), func(t *testing.T) {
				f, dir := newFake(), t.TempDir()
				d := draft
				d.ReportID = NewReportID()
				// Equal basenames exercise the same collision handling on retry.
				d.Screenshots = append(shots(t, 1, ext, 10), shots(t, 1, ext, 10)...)
				f.fail["artifact create"] = true
				if _, err := Submit(context.Background(), f.exec, dir, d); err == nil {
					t.Fatal("want artifact failure")
				}
				if savedPaths {
					s, err := load(filepath.Join(dir, d.ReportID, "state.json"))
					if err != nil {
						t.Fatal(err)
					}
					d = s.Draft
				}
				f.fail["artifact create"] = false
				r, err := Submit(context.Background(), f.exec, dir, d)
				if err != nil || r.IssueURL == "" || f.count(create) != 1 || f.count("issue comment") != 0 ||
					f.count("issue artifact create 7 --body-file - --name screenshots.md -R projectbluefin/dakota --attach") != 1 {
					t.Fatal("unchanged retry failed", r, err, f.calls)
				}
				// Published retries need neither originals nor the deleted draft copies.
				for _, p := range d.Screenshots {
					if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				}
				calls := len(f.calls)
				if again, err := Submit(context.Background(), f.exec, dir, d); err != nil || again != r || len(f.calls) != calls {
					t.Fatal("published retry was not idempotent", again, err, f.calls[calls:])
				}
			})
		}
	}
}

func TestSubmitDistinguishesMissingAndEmptyAttachment(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := draft
	d.ReportID, d.Attachment = NewReportID(), []byte{}
	s := &Saved{ReportID: d.ReportID, Stage: Reviewed, Draft: d}
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	d.Attachment = nil
	if _, err := Submit(context.Background(), f.exec, dir, d); err == nil {
		t.Fatal("removing an empty attachment must require explicit review")
	}
	if f.count("api")+f.count("gist")+f.count("issue") != 0 {
		t.Fatal("attachment mismatch caused remote work", f.calls)
	}
}

func TestSubmitPreservesCorruptState(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := draft
	d.ReportID = NewReportID()
	reportDir := filepath.Join(dir, d.ReportID)
	if err := os.MkdirAll(reportDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(reportDir, "state.json")
	before := []byte(`{"ReportID":`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Submit(context.Background(), f.exec, dir, d)
	var syntaxError *json.SyntaxError
	if !errors.As(err, &syntaxError) {
		t.Fatalf("want original corrupt-state error, got %v", err)
	}
	if f.count("api")+f.count("gist")+f.count("issue") != 0 {
		t.Fatal("corrupt state caused remote work", f.calls)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("corrupt state was overwritten", err)
	}
}

func TestSubmitPreservesUnreadableState(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := draft
	d.ReportID = NewReportID()
	path := filepath.Join(dir, d.ReportID, "state.json")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Submit(context.Background(), f.exec, dir, d)
	var pathError *os.PathError
	if !errors.As(err, &pathError) || pathError.Op != "read" {
		t.Fatalf("want original unreadable-state error, got %v", err)
	}
	if f.count("api")+f.count("gist")+f.count("issue") != 0 {
		t.Fatal("unreadable state caused remote work", f.calls)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatal("unreadable state was replaced", err)
	}
}

func TestUncertainResume(t *testing.T) {
	for _, found := range []bool{true, false} {
		f, dir := newFake(), t.TempDir()
		f.fail["issue create"] = true
		if _, err := Submit(context.Background(), f.exec, dir, draft); err == nil {
			t.Fatal("want error")
		}
		p, _ := Pending(dir)
		if len(p) != 1 || p[0].Stage != Uncertain || p[0].StartedAt.IsZero() || p[0].Draft.Attachment == nil {
			t.Fatal(p)
		}
		f.fail["issue create"] = false
		f.fail["search issues"] = true // search index lags; must not be used
		if found {
			f.out["api issues"] = "https://github.com/projectbluefin/dakota/issues/9\n"
			f.out["issue view"] = "kind/bug\n"
		}
		r, err := Resume(context.Background(), f.exec, dir, p[0].ReportID)
		if err != nil {
			t.Fatal(err)
		}
		creates := f.count(create)
		if found && (creates != 1 || !strings.HasSuffix(r.IssueURL, "/9") || f.count("issue artifact create 9 ") != 1) {
			t.Fatal("found: ", f.calls, r)
		}
		if !found && (creates != 2 || !strings.HasSuffix(r.IssueURL, "/7") || f.count(artifact) != 1) {
			t.Fatal("not found: ", f.calls, r)
		}
		if f.count("gist") != 0 || f.count("api --paginate gists") != 0 || r.GistURL != "" {
			t.Fatal(f.calls, r)
		}
		if f.count("api --paginate repos/projectbluefin/dakota/issues?creator=me&state=all&per_page=100&since=") != 1 {
			t.Fatal(f.calls)
		}
	}
}

func TestResumeAfterCrashBeforeArtifact(t *testing.T) {
	for _, exists := range []bool{false, true} {
		f, dir := newFake(), t.TempDir()
		if err := save(dir, &Saved{ReportID: "r1", Stage: LabelsDone, Draft: draft, IssueURL: "https://github.com/projectbluefin/dakota/issues/7"}); err != nil {
			t.Fatal(err)
		}
		f.out["artifact list"] = "other.txt\n"
		if exists {
			f.out["artifact list"] = "other.txt\nsystemd-report.json\n"
		}
		if _, err := Resume(context.Background(), f.exec, dir, "r1"); err != nil {
			t.Fatal(err)
		}
		want := 1
		if exists {
			want = 0
		}
		if f.count(create) != 0 || f.count("issue artifact list 7 -R projectbluefin/dakota") != 1 || f.count(artifact) != want || f.count("gist") != 0 {
			t.Fatal(exists, f.calls)
		}
		if p, _ := Pending(dir); len(p) != 0 {
			t.Fatal(p)
		}
	}
}

func TestConfirm(t *testing.T) {
	f := newFake()
	r, err := Submit(context.Background(), f.exec, t.TempDir(), Draft{Mode: Confirm, Repo: "projectbluefin/dakota", Body: "same here", IssueRef: "#3"})
	if err != nil || f.count("gist") != 0 || f.count("issue artifact") != 0 || f.count("issue comment --repo projectbluefin/dakota --body-file - -- 3") != 1 || f.count(create) != 0 ||
		r.IssueURL != "https://github.com/projectbluefin/dakota/issues/3" || strings.Contains(f.in("issue comment"), "Full report") {
		t.Fatal(f.calls, r, err)
	}
}

func TestConfirmWithReport(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := Draft{ReportID: "abc", Mode: Confirm, Repo: "projectbluefin/dakota", Body: "Same problem here.", IssueRef: "3", Attachment: []byte("{}")}
	if _, err := Submit(context.Background(), f.exec, dir, d); err != nil {
		t.Fatal(err)
	}
	body := f.in("issue comment")
	if f.count("issue artifact create 3 --body-file - --name systemd-report-abc.json") != 1 || f.count("issue comment") != 1 || f.count("gist") != 0 ||
		!strings.Contains(body, "`systemd-report-abc.json`, attached to this issue") || !strings.Contains(body, "<!-- bluefin-report-id: abc -->") ||
		!strings.Contains(body, "<!-- bluefin-report-confirm -->") {
		t.Fatal(f.calls, body)
	}
	if p, _ := Pending(dir); len(p) != 0 {
		t.Fatal(p)
	}
}

func TestConfirmArtifactRefused(t *testing.T) {
	f := newFake()
	f.fail["artifact create"] = true
	f.errs = map[string]string{"artifact create": "HTTP 403"}
	d := Draft{ReportID: "abc", Mode: Confirm, Repo: "projectbluefin/dakota", Body: "x", IssueRef: "3", Attachment: []byte("{}")}
	r, err := Submit(context.Background(), f.exec, t.TempDir(), d)
	if err != nil || r.GistURL != "https://gist.github.com/abc" || f.count("gist create --desc bluefin-report abc --filename systemd-report-abc.json -") != 1 ||
		f.count("issue comment") != 1 || !strings.Contains(f.in("issue comment"), "Full report: https://gist.github.com/abc") {
		t.Fatal(err, r, f.calls)
	}
}

func TestConfirmResumeAfterArtifact(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.fail["issue comment"] = true // crash between artifact and comment
	d := Draft{ReportID: "abc", Mode: Confirm, Repo: "projectbluefin/dakota", Body: "x", IssueRef: "3", Attachment: []byte("{}")}
	if _, err := Submit(context.Background(), f.exec, dir, d); err == nil {
		t.Fatal("comment failure should be reported")
	}
	f.fail["issue comment"] = false
	f.out["artifact list"] = "systemd-report.json\nsystemd-report-abc.json\n"
	if _, err := Resume(context.Background(), f.exec, dir, "abc"); err != nil {
		t.Fatal(err)
	}
	// Second run started at CommentPending: no artifact listing or re-upload, the comment checked then posted once.
	if f.count("issue artifact create") != 1 || f.count("issue comment") != 2 || f.count("api --paginate repos/projectbluefin/dakota/issues/3/comments") != 1 {
		t.Fatal(f.calls)
	}
	// Crash before the artifact step finished: Resume lists by name and skips re-creating.
	f, dir = newFake(), t.TempDir()
	f.fail["artifact create"] = true
	Submit(context.Background(), f.exec, dir, d)
	f.fail["artifact create"] = false
	f.out["artifact list"] = "systemd-report-abc.json\n"
	if _, err := Resume(context.Background(), f.exec, dir, "abc"); err != nil || f.count("issue artifact create") != 1 || f.count("issue comment") != 1 {
		t.Fatal(err, f.calls)
	}
}

func TestConfirmUncertainFoundComment(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.fail["issue comment"] = true
	d := Draft{Mode: Confirm, Repo: "projectbluefin/dakota", Body: "x", IssueRef: "https://github.com/projectbluefin/dakota/issues/3"}
	Submit(context.Background(), f.exec, dir, d)
	p, _ := Pending(dir)
	f.out["api comments"] = "5\n"
	r, err := Resume(context.Background(), f.exec, dir, p[0].ReportID)
	if err != nil || f.count("issue comment") != 1 || !strings.HasSuffix(r.IssueURL, "/3") || f.count("api --paginate repos/projectbluefin/dakota/issues/3/comments?") != 1 {
		t.Fatal(err, f.calls, r)
	}
}

func TestNotSignedIn(t *testing.T) {
	f := newFake()
	f.fail["auth status"] = true
	if _, err := Submit(context.Background(), f.exec, t.TempDir(), draft); !errors.Is(err, ErrNotSignedIn) {
		t.Fatal(err)
	}
}

func TestResumeNotSignedIn(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.fail["issue create"] = true
	Submit(context.Background(), f.exec, dir, draft)
	p, _ := Pending(dir)
	f = newFake()
	f.fail["auth status"] = true
	if _, err := Resume(context.Background(), f.exec, dir, p[0].ReportID); !errors.Is(err, ErrNotSignedIn) || len(f.calls) != 1 {
		t.Fatal(err, f.calls)
	}
}

func TestResumeRejectsRepoOutsideAllowlist(t *testing.T) {
	dir := t.TempDir()
	if err := save(dir, &Saved{ReportID: "abc123", Stage: Reviewed, Draft: Draft{Repo: "projectbluefin/bluefin", Title: "t", Body: "b"}}); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	if _, err := Resume(context.Background(), f.exec, dir, "abc123"); err == nil || len(f.calls) != 0 {
		t.Fatal("old-routing draft resumed:", err, f.calls)
	}
}

func TestIssueRef(t *testing.T) {
	const repo = "projectbluefin/dakota"
	for _, ok := range []string{"12", "#12", "https://github.com/projectbluefin/dakota/issues/12", "https://github.com/ProjectBluefin/Dakota/issues/12#issuecomment-1"} {
		if n, err := ParseIssueRef(repo, ok); n != 12 || err != nil {
			t.Error(ok, n, err)
		}
	}
	for _, bad := range []string{"--web", "-1", "0", "#", "", "abc", "12 --web", "https://github.com/evil/repo/issues/12", "https://github.com/projectbluefin/utah/issues/12", "https://github.com/projectbluefin/dakota/pull/12"} {
		f := newFake()
		if _, err := Submit(context.Background(), f.exec, t.TempDir(), Draft{Mode: Confirm, Repo: repo, Body: "x", IssueRef: bad}); err == nil || len(f.calls) != 0 {
			t.Error(bad, err, f.calls)
		}
	}
}

func TestSignIn(t *testing.T) {
	var args []string
	gh := func(_ context.Context, _ []byte, on func(string), a ...string) error {
		args = a
		for _, l := range []string{
			"! First copy your one-time code: AB12-CD34",
			"Open this URL to continue in your web browser: https://github.com/login/device",
			"✓ Authentication complete.",
			"https://github.com/login/device",
		} {
			on(l)
		}
		return nil
	}
	n := 0
	err := SignIn(context.Background(), gh, func(code, url string) {
		n++
		if code != "AB12-CD34" || url != "https://github.com/login/device" {
			t.Fatal(code, url)
		}
	})
	if err != nil || n != 1 || strings.Join(args, " ") != "auth login --hostname github.com --git-protocol https --web --skip-ssh-key" {
		t.Fatal(err, n, args)
	}
}

func TestDiscard(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.fail["issue create"] = true
	Submit(context.Background(), f.exec, dir, draft)
	p, _ := Pending(dir)
	if err := Discard(dir, p[0].ReportID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, p[0].ReportID)); !os.IsNotExist(err) || Discard(dir, "..") == nil {
		t.Fatal("not removed", err)
	}
}

func TestLabelsAndArtifact(t *testing.T) {
	for _, maintainer := range []bool{true, false} {
		f := newFake()
		f.out["issue create"] = "https://github.com/projectbluefin/dakota/issues/7 kind/bug,area/gaming\n"
		if !maintainer {
			f.out["issue create"] = "https://github.com/projectbluefin/dakota/issues/7 needs-triage\n"
		}
		d := draft
		d.Area = "gaming"
		if _, err := Submit(context.Background(), f.exec, t.TempDir(), d); err != nil {
			t.Fatal(err)
		}
		if f.count(create+" -f title=t -F body=@- -f labels[]=kind/bug -f labels[]=area/gaming -f type=Bug") != 1 {
			t.Fatal(f.calls)
		}
		slash := f.count("issue comment --repo projectbluefin/dakota --body-file - -- 7")
		if maintainer && slash != 0 || !maintainer && (slash != 1 || !slices.Contains(f.stdin, "/kind bug\n/area gaming")) {
			t.Fatal(maintainer, f.calls, f.stdin)
		}
		if f.count(artifact) != 1 {
			t.Fatal(f.calls)
		}
	}
}

func TestNoAttachmentNoArtifact(t *testing.T) {
	f := newFake()
	d := draft
	d.Attachment = nil
	if _, err := Submit(context.Background(), f.exec, t.TempDir(), d); err != nil || f.count("issue artifact") != 0 || f.count("gist") != 0 ||
		strings.Contains(f.in(create), "Full report") {
		t.Fatal(err, f.calls)
	}
}

func TestResumeAfterArtifactFailure(t *testing.T) {
	for _, exists := range []bool{false, true} {
		f, dir := newFake(), t.TempDir()
		f.out["issue create"] = "https://github.com/projectbluefin/dakota/issues/7 \n" // labels dropped
		f.fail["artifact create"] = true
		r, err := Submit(context.Background(), f.exec, dir, draft)
		p, _ := Pending(dir)
		if err == nil || r.IssueURL == "" || len(p) != 1 || p[0].Stage != LabelsDone {
			t.Fatal(err, r, p)
		}
		f.fail["artifact create"] = false
		f.out["issue comment"] = "" // comment already posted by us
		if exists {
			f.out["artifact list"] = "systemd-report.json\n"
		}
		if _, err := Resume(context.Background(), f.exec, dir, p[0].ReportID); err != nil {
			t.Fatal(err)
		}
		want := 2
		if exists {
			want = 1
		}
		if f.count(create) != 1 || f.count("issue comment") != 1 || f.count("issue artifact create") != want || f.count("gist") != 0 {
			t.Fatal(exists, f.calls)
		}
		if p, _ := Pending(dir); len(p) != 0 {
			t.Fatal(p)
		}
	}
}

func TestResumeAfterCrashPostIssue(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	// Crash right after create: state says IssueCreated, slash comment already on GitHub.
	if err := save(dir, &Saved{ReportID: "r1", Stage: IssueCreated, Draft: draft,
		IssueURL: "https://github.com/projectbluefin/dakota/issues/7"}); err != nil {
		t.Fatal(err)
	}
	f.out["issue view"] = "needs-triage\n"
	f.out["api comments"] = "123\n"
	if _, err := Resume(context.Background(), f.exec, dir, "r1"); err != nil {
		t.Fatal(err)
	}
	if f.count(create) != 0 || f.count("issue comment") != 0 || f.count(artifact) != 1 {
		t.Fatal(f.calls)
	}
}

func TestArtifactRefusedFallsBackToGist(t *testing.T) {
	for _, stderr := range []string{"HTTP 403: Resource not accessible by integration", `unknown command "artifact" for "gh issue"`} {
		f, dir := newFake(), t.TempDir()
		f.fail["artifact create"] = true
		f.errs = map[string]string{"artifact create": stderr}
		d := draft
		d.ReportID = NewReportID()
		r, err := Submit(context.Background(), f.exec, dir, d)
		if p, _ := Pending(dir); err != nil || r.IssueURL == "" || r.GistURL != "https://gist.github.com/abc" || len(p) != 0 {
			t.Fatalf("%q: err=%v r=%v pending=%v", stderr, err, r, p)
		}
		link := "Full report: https://gist.github.com/abc\n<!-- bluefin-report-id: " + d.ReportID + " -->"
		if f.count("gist create --desc bluefin-report "+d.ReportID+" --filename systemd-report.json -") != 1 || f.in("gist create") != "{}" ||
			f.count("issue comment --repo projectbluefin/dakota --body-file - -- 7") != 1 || f.in("issue comment") != link || f.count("api --paginate") != 0 {
			t.Fatal(stderr, f.calls, f.stdin)
		}
	}
	// A transient failure keeps it pending for Resume, without a gist.
	f, dir := newFake(), t.TempDir()
	f.fail["artifact create"] = true
	d := draft
	d.ReportID = NewReportID()
	if _, err := Submit(context.Background(), f.exec, dir, d); err == nil {
		t.Fatal("transient artifact failure should be reported")
	}
	if p, _ := Pending(dir); len(p) != 1 || p[0].Stage != LabelsDone || p[0].Draft.Attachment == nil || f.count("gist") != 0 {
		t.Fatal("transient artifact failure should stay pending", p, f.calls)
	}
}

func TestFallbackResumeNeverDuplicates(t *testing.T) {
	for _, posted := range []bool{false, true} {
		f, dir := newFake(), t.TempDir()
		f.fail["artifact create"], f.fail["gist create"] = true, true
		f.errs = map[string]string{"artifact create": "HTTP 403"}
		Submit(context.Background(), f.exec, dir, draft)
		p, _ := Pending(dir)
		if len(p) != 1 || p[0].Stage != Uncertain || p[0].IssueURL == "" {
			t.Fatal(p, f.calls)
		}
		// gh timed out after GitHub stored the gist: Resume finds it via REST instead of uploading again.
		f.fail["gist create"] = false
		f.out["api gists"] = "https://gist.github.com/abc\n"
		if posted {
			f.out["api comments"] = "55\n"
		}
		r, err := Resume(context.Background(), f.exec, dir, p[0].ReportID)
		if err != nil || r.GistURL != "https://gist.github.com/abc" {
			t.Fatal(err, r)
		}
		want := 1
		if posted {
			want = 0
		}
		if f.count(create) != 1 || f.count("gist create") != 1 || f.count("issue comment") != want || f.count("api --paginate repos/projectbluefin/dakota/issues/7/comments") != 1 {
			t.Fatal(posted, f.calls)
		}
		if p, _ := Pending(dir); len(p) != 0 {
			t.Fatal(p)
		}
	}
}

// Account binding: an uncertain write can only be resolved under the account that started it.

// TestSubmitPersistsAccount checks the authenticated identity is recorded before the first write.
func TestSubmitPersistsAccount(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := draft
	d.ReportID = NewReportID()
	if _, err := Submit(context.Background(), f.exec, dir, d); err != nil {
		t.Fatal(err)
	}
	if f.count("api user") != 1 {
		t.Fatal("expected one api user call to capture the account", f.calls)
	}
	s, err := load(filepath.Join(dir, d.ReportID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Account != "me" {
		t.Fatalf("saved state has no account: %+v", s)
	}
}

// TestResumeAccountMismatchStopsWithoutWrite is the core fix: resuming an uncertain write under a
// different gh account must stop with guidance and perform no remote write, so it cannot duplicate
// a write that actually succeeded but timed out.
func TestResumeAccountMismatchStopsWithoutWrite(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.fail["issue create"] = true
	if _, err := Submit(context.Background(), f.exec, dir, draft); err == nil {
		t.Fatal("want first issue write to fail")
	}
	p, _ := Pending(dir)
	if len(p) != 1 || p[0].Stage != Uncertain || p[0].Account != "me" {
		t.Fatal("uncertain draft must record the starting account", p)
	}

	// Switch gh to another account, and let a lookup there report nothing (the bug that would have
	// made resume duplicate the write). Resume must refuse instead.
	f = newFake()
	f.out["api user"] = "someone-else\n"
	f.out["api issues"] = "" // nothing created under the new account
	_, err := Resume(context.Background(), f.exec, dir, p[0].ReportID)
	if err == nil || !strings.Contains(err.Error(), "someone-else") || !strings.Contains(err.Error(), "me") {
		t.Fatal("want an account-mismatch error naming both accounts", err, f.calls)
	}
	if f.count("api user") != 1 {
		t.Fatal("expected the account check to run", f.calls)
	}
	if f.count(create) != 0 || f.count("api --paginate repos/projectbluefin/dakota/issues?creator=") != 0 {
		t.Fatal("account mismatch must not look up or re-create the write", f.calls)
	}
	s, err := load(filepath.Join(dir, p[0].ReportID, "state.json"))
	if err != nil || s.Stage != Uncertain || s.Account != "me" {
		t.Fatal("mismatch resume must not modify saved state", s, err)
	}
}

// TestResumeSameAccountProceeds confirms the fix does not break a legitimate same-account resume.
func TestResumeSameAccountProceeds(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.fail["issue create"] = true
	if _, err := Submit(context.Background(), f.exec, dir, draft); err == nil {
		t.Fatal("want first issue write to fail")
	}
	p, _ := Pending(dir)
	f.fail["issue create"] = false
	f.out["api issues"] = "https://github.com/projectbluefin/dakota/issues/9\n"
	if _, err := Resume(context.Background(), f.exec, dir, p[0].ReportID); err != nil {
		t.Fatal(err)
	}
	if f.count("api --paginate repos/projectbluefin/dakota/issues?creator=me&state=all&per_page=100") != 1 {
		t.Fatal("same-account resume must resolve the write through lookup", f.calls)
	}
}

// TestResumeUnboundDraftFallsBackToLookup keeps older drafts (saved before account binding) working.
func TestResumeUnboundDraftFallsBackToLookup(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	if err := save(dir, &Saved{ReportID: "r1", Stage: Uncertain, Account: "", Draft: draft}); err != nil {
		t.Fatal(err)
	}
	f.out["api issues"] = "https://github.com/projectbluefin/dakota/issues/9\n"
	if _, err := Resume(context.Background(), f.exec, dir, "r1"); err != nil {
		t.Fatal(err)
	}
	if f.count("api --paginate repos/projectbluefin/dakota/issues?creator=me&state=all&per_page=100") != 1 {
		t.Fatal("an unbound draft must still resolve through the plain lookup", f.calls)
	}
}

// TestAccountLookupFailureFailsClosed: if gh cannot say who is signed in, neither Submit nor Resume writes.
func TestAccountLookupFailureFailsClosed(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.out["api user"] = "\n"
	if _, err := Submit(context.Background(), f.exec, dir, draft); !errors.Is(err, ErrNotSignedIn) || f.count(create) != 0 {
		t.Fatal("empty login must not start a write", err, f.calls)
	}
	f = newFake()
	f.fail["issue create"] = true
	Submit(context.Background(), f.exec, dir, draft)
	p, _ := Pending(dir)
	f = newFake()
	f.fail["api user"] = true
	if _, err := Resume(context.Background(), f.exec, dir, p[0].ReportID); err == nil || f.count(create) != 0 || f.count("api --paginate") != 0 {
		t.Fatal("failed account lookup must stop resume before any lookup or write", err, f.calls)
	}
}

func shots(t *testing.T, n int, ext string, size int) []string {
	src := t.TempDir()
	var ps []string
	for i := range n {
		p := filepath.Join(src, strings.Repeat("s", i+1)+ext)
		var b bytes.Buffer // a real image: Submit re-encodes screenshots
		img := image.NewGray(image.Rect(0, 0, 1, 1))
		if ext == ".jpg" {
			jpeg.Encode(&b, img, nil)
		} else {
			png.Encode(&b, img)
		}
		if err := os.WriteFile(p, append(b.Bytes(), make([]byte, max(0, size-b.Len()))...), 0o644); err != nil {
			t.Fatal(err)
		}
		ps = append(ps, p)
	}
	return ps
}

func TestScreenshots(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := draft
	d.ReportID = NewReportID()
	d.Screenshots = shots(t, 2, ".png", 10)
	if _, err := Submit(context.Background(), f.exec, dir, d); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dir, d.ReportID, "screenshots")
	want := "issue artifact create 7 --body-file - --name screenshots.md -R projectbluefin/dakota --attach " + filepath.Join(copied, "s.png") + " --attach " + filepath.Join(copied, "ss.png")
	if f.count(want) != 1 || f.in(want) != "Screenshots\n" || f.count("issue comment") != 0 || f.count(artifact) != 1 {
		t.Fatal(f.calls)
	}
	if _, err := os.Stat(copied); !os.IsNotExist(err) {
		t.Fatal("copied screenshots not removed when Published", err)
	}

	f = newFake()
	d.ReportID, d.Screenshots = NewReportID(), nil
	if _, err := Submit(context.Background(), f.exec, dir, d); err != nil || f.count("issue comment") != 0 || f.count("issue artifact create 7 --body-file - --name screenshots") != 0 {
		t.Fatal(err, f.calls)
	}
}

// A screenshots artifact that already exists (crash after create) is found by name, not uploaded again.
func TestScreenshotsResume(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := draft
	d.ReportID = NewReportID()
	d.Screenshots = shots(t, 1, ".gif", 10)
	f.fail["artifact create"] = true
	if _, err := Submit(context.Background(), f.exec, dir, d); err == nil {
		t.Fatal("want warning")
	}
	s, _ := load(filepath.Join(dir, d.ReportID, "state.json"))
	if s.Stage != LabelsDone {
		t.Fatal(s.Stage)
	}
	os.Remove(d.Screenshots[0]) // originals may move; the draft copy is used
	if fi, err := os.Stat(s.Draft.Screenshots[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatal("not copied 0600", err)
	}
	f = newFake()
	f.out["artifact list"] = "systemd-report.json\nscreenshots.md\n"
	if _, err := Resume(context.Background(), f.exec, dir, d.ReportID); err != nil || f.count("issue artifact create") != 0 || f.count("issue comment") != 0 {
		t.Fatal(err, f.calls)
	}
	if p, _ := Pending(dir); len(p) != 0 {
		t.Fatal(p)
	}
}

// When GitHub refuses artifacts, screenshots fall back to one comment on the issue.
func TestScreenshotsArtifactsRefused(t *testing.T) {
	for _, withReport := range []bool{true, false} {
		f, dir := newFake(), t.TempDir()
		d := draft
		d.ReportID = NewReportID()
		d.Screenshots = shots(t, 1, ".png", 10)
		gists, creates := 1, 1
		if !withReport { // screenshots only: refusal comes from the screenshots artifact, and no gist is needed
			d.Attachment, gists = nil, 0
		}
		f.fail["artifact create"], f.errs = true, map[string]string{"artifact create": "HTTP 403"}
		if _, err := Submit(context.Background(), f.exec, dir, d); err != nil {
			t.Fatal(withReport, err, f.calls)
		}
		marker := "<!-- bluefin-report-screenshots: " + d.ReportID + " -->"
		if f.count("issue artifact create") != creates || f.count("gist create") != gists ||
			!slices.ContainsFunc(f.stdin, func(in string) bool { return in == "Screenshots\n"+marker }) ||
			f.count("issue comment --repo projectbluefin/dakota --body-file - --attach ") != 1 {
			t.Fatal(withReport, f.calls)
		}
		if p, _ := Pending(dir); len(p) != 0 {
			t.Fatal(withReport, "not Published", p)
		}
	}
}

func TestResumeRefusesDraftEditedOnDisk(t *testing.T) {
	for _, edit := range []string{"body", "screenshot"} {
		f, dir := newFake(), t.TempDir()
		d := draft
		d.ReportID = NewReportID()
		d.Screenshots = shots(t, 1, ".gif", 10)
		f.fail["artifact create"] = true // leave an unfinished draft
		Submit(context.Background(), f.exec, dir, d)
		state := filepath.Join(dir, d.ReportID, "state.json")
		s, _ := load(state)
		if edit == "body" {
			s.Draft.Body += " edited"
			b, _ := json.Marshal(s)
			os.WriteFile(state, b, 0o600)
		} else {
			os.WriteFile(s.Draft.Screenshots[0], []byte("other image"), 0o600)
		}
		before, _ := os.ReadFile(state)
		f = newFake()
		if _, err := Resume(context.Background(), f.exec, dir, d.ReportID); !errors.Is(err, ErrDraftChanged) || len(f.calls) != 0 {
			t.Fatalf("%s: err=%v calls=%v", edit, err, f.calls)
		}
		if after, _ := os.ReadFile(state); !bytes.Equal(before, after) {
			t.Fatalf("%s: edited draft was overwritten", edit)
		}
	}
}

func TestPendingListsReadableDraftsBesideCorruptOne(t *testing.T) {
	dir := t.TempDir()
	if err := save(dir, &Saved{ReportID: "good", Stage: LabelsDone, Draft: draft}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "bad"), 0o700)
	os.WriteFile(filepath.Join(dir, "bad", "state.json"), []byte(`{"ReportID":`), 0o600)
	p, err := Pending(dir)
	if err == nil || len(p) != 1 || p[0].ReportID != "good" {
		t.Fatal(p, err)
	}
}

func TestScreenshotsConfirm(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := Draft{Mode: Confirm, Repo: "projectbluefin/dakota", Body: "b", IssueRef: "3", Screenshots: shots(t, 1, ".jpg", 10)}
	if _, err := Submit(context.Background(), f.exec, dir, d); err != nil {
		t.Fatal(err)
	}
	shots := slices.IndexFunc(f.calls, func(c string) bool {
		return strings.HasPrefix(c, "issue artifact create 3 --body-file - --name screenshots-")
	})
	last := f.calls[len(f.calls)-1]
	if shots < 0 || !strings.HasSuffix(f.calls[shots], "s.jpg") || f.stdin[shots] != "Screenshots\n" ||
		f.count("issue comment") != 1 || strings.Contains(last, "--attach") || !strings.HasSuffix(last, "-- 3") ||
		strings.Contains(f.in("issue comment"), "bluefin-report-screenshots") {
		t.Fatal(f.calls)
	}
}

func TestScreenshotLimits(t *testing.T) {
	for name, ps := range map[string][]string{
		"count": shots(t, 11, ".png", 1),
		"size":  shots(t, 1, ".png", MaxScreenshotBytes+1),
		"ext":   shots(t, 1, ".exe", 1),
	} {
		f := newFake()
		d := draft
		d.Screenshots = ps
		if _, err := Submit(context.Background(), f.exec, t.TempDir(), d); err == nil || len(f.calls) != 0 {
			t.Errorf("%s: err=%v calls=%v", name, err, f.calls)
		}
	}
}

func TestUnfinishedDraftIsNeverPublished(t *testing.T) {
	dir := t.TempDir()
	d := Draft{ReportID: "r1", Repo: "projectbluefin/dakota", Title: "T", Body: "half", Steps: "s"}
	if _, err := SaveUnfinished(dir, d); err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	if _, err := Resume(context.Background(), f.exec, dir, "r1"); !errors.Is(err, ErrUnfinished) {
		t.Fatalf("Resume err = %v, want ErrUnfinished", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("gh was called: %v", f.calls)
	}
	if ds, _ := Pending(dir); len(ds) != 1 || ds[0].Draft.Steps != "s" {
		t.Fatalf("pending = %+v", ds)
	}
	if err := Discard(dir, "r1"); err != nil {
		t.Fatal(err)
	}
	if ds, _ := Pending(dir); len(ds) != 0 {
		t.Fatalf("pending after discard = %+v", ds)
	}
}

func TestSaveUnfinishedKeepsReviewedReport(t *testing.T) {
	dir := t.TempDir()
	s := &Saved{ReportID: "r2", Stage: Reviewed, Draft: Draft{ReportID: "r2", Repo: "projectbluefin/dakota"}}
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveUnfinished(dir, Draft{ReportID: "r2", Repo: "projectbluefin/dakota", Body: "x"}); err == nil {
		t.Fatal("SaveUnfinished overwrote a reviewed report")
	}
	if _, err := SaveUnfinished(dir, Draft{ReportID: "../x"}); err == nil {
		t.Fatal("SaveUnfinished accepted a bad id")
	}
}

func TestSaveUnfinishedKeepsEarlierScreenshot(t *testing.T) {
	dir := t.TempDir()
	first := shots(t, 1, ".png", 10)[0]
	d := Draft{ReportID: "r3", Repo: "projectbluefin/dakota", Screenshots: []string{first}}
	saved, err := SaveUnfinished(dir, d)
	if err != nil || len(saved) != 1 {
		t.Fatalf("saved = %v, %v", saved, err)
	}
	kept, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(t.TempDir(), filepath.Base(first)) // same upload name, other picture
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, 2, 2)))
	if err := os.WriteFile(second, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	d.Screenshots = []string{saved[0], second}
	again, err := SaveUnfinished(dir, d)
	if err != nil || len(again) != 2 || again[0] != saved[0] || again[1] == again[0] {
		t.Fatalf("again = %v, %v", again, err)
	}
	if got, _ := os.ReadFile(again[0]); !bytes.Equal(got, kept) {
		t.Fatal("re-saving overwrote the kept screenshot copy")
	}
	if err := os.Remove(again[0]); err != nil { // e.g. the draft was discarded meanwhile
		t.Fatal(err)
	}
	if last, err := SaveUnfinished(dir, Draft{ReportID: "r3", Screenshots: again}); err != nil || !slices.Equal(last, again[1:]) {
		t.Fatalf("missing copy kept: %v, %v", last, err)
	}
}
