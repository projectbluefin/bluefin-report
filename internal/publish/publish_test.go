package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fake struct {
	calls []string
	fail  map[string]bool // keyed by "gh sub" or "api gists|issues|comments|user"
	out   map[string]string
}

func (f *fake) exec(_ context.Context, _ []byte, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	k := args[0] + " " + args[1]
	if args[0] == "api" {
		p := args[1]
		if p == "--paginate" {
			p = args[2]
		}
		switch {
		case strings.Contains(p, "/comments"):
			k = "api comments"
		case strings.Contains(p, "/issues"):
			k = "api issues"
		default:
			k = "api " + strings.SplitN(p, "?", 2)[0]
		}
	}
	if f.fail[k] {
		return nil, []byte("boom"), errors.New("exit 1")
	}
	return []byte(f.out[k]), nil, nil
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
		"issue create":  "Creating issue\nhttps://github.com/projectbluefin/bluefin/issues/7\n",
		"issue comment": "https://github.com/projectbluefin/bluefin/issues/3#c1\n",
		"api user":      "me\n",
	}}
}

var draft = Draft{Repo: "projectbluefin/bluefin", Title: "t", Body: "b", Attachment: []byte("{}"), Queue: "3-human-queue"}

func TestRoute(t *testing.T) {
	for id, want := range map[string]string{"bluefin-lts": "projectbluefin/bluefin-lts", "bluefin-dx": "projectbluefin/bluefin", "dakota": "projectbluefin/dakota", "fedora": "projectbluefin/common"} {
		if got := Route(id); got != want {
			t.Errorf("Route(%q)=%q", id, got)
		}
	}
	f := newFake()
	if _, err := Submit(context.Background(), f.exec, t.TempDir(), Draft{Repo: "evil/bluefin"}); err == nil || len(f.calls) != 0 {
		t.Fatal("non-projectbluefin repo not rejected before network")
	}
}

func TestSuccess(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	d := draft
	d.ReportID = NewReportID()
	r, err := Submit(context.Background(), f.exec, dir, d)
	if err != nil || r.GistURL != "https://gist.github.com/abc" || !strings.HasSuffix(r.IssueURL, "/issues/7") {
		t.Fatal(r, err)
	}
	if f.count("gist create") != 1 || f.count("issue create") != 1 {
		t.Fatal(f.calls)
	}
	if p, _ := Pending(dir); len(p) != 0 {
		t.Fatal("not Published", p)
	}
	s, err := load(filepath.Join(dir, d.ReportID, "state.json"))
	if err != nil || s.Draft.Attachment != nil || s.Draft.Body != "" || s.IssueURL == "" {
		t.Fatalf("published draft still holds the payload or lost its URL: %+v %v", s, err)
	}
	if r2, err := Submit(context.Background(), f.exec, dir, d); err != nil || r2 != r || f.count("issue create") != 1 {
		t.Fatal("re-submit of a published report must return the stored result", r2, err, f.calls)
	}
}

func TestGistFailureResumesWithoutDuplicate(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.fail["gist create"] = true
	d := draft
	d.ReportID = NewReportID()
	if _, err := Submit(context.Background(), f.exec, dir, d); err == nil {
		t.Fatal("want error")
	}
	p, _ := Pending(dir)
	if f.count("issue") != 0 || len(p) != 1 || p[0].Stage != Uncertain {
		t.Fatal(f.calls, p)
	}
	// The gist was actually created before gh timed out; the retry must find it via REST, not upload again.
	f.fail["gist create"] = false
	f.out["api gists"] = "https://gist.github.com/abc\n"
	r, err := Submit(context.Background(), f.exec, dir, d)
	if err != nil {
		t.Fatal(err)
	}
	if f.count("gist create") != 1 || f.count("issue create") != 1 || r.IssueURL == "" {
		t.Fatal(f.calls, r)
	}
	// Submitting a published report again is a no-op.
	if _, err := Submit(context.Background(), f.exec, dir, d); err != nil || f.count("issue create") != 1 {
		t.Fatal(err, f.calls)
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
		if len(p) != 1 || p[0].Stage != Uncertain || p[0].StartedAt.IsZero() {
			t.Fatal(p)
		}
		f.fail["issue create"] = false
		f.fail["search issues"] = true // search index lags; must not be used
		if found {
			f.out["api issues"] = "https://github.com/projectbluefin/bluefin/issues/9\n"
		}
		r, err := Resume(context.Background(), f.exec, dir, p[0].ReportID)
		if err != nil {
			t.Fatal(err)
		}
		creates := f.count("issue create")
		if found && (creates != 1 || !strings.HasSuffix(r.IssueURL, "/9")) {
			t.Fatal("found: ", f.calls, r)
		}
		if !found && (creates != 2 || !strings.HasSuffix(r.IssueURL, "/7")) {
			t.Fatal("not found: ", f.calls, r)
		}
		if f.count("gist create") != 1 || f.count("api --paginate gists") != 0 || r.GistURL != "https://gist.github.com/abc" {
			t.Fatal(f.calls, r)
		}
		if f.count("api --paginate repos/projectbluefin/bluefin/issues?creator=me&state=all&per_page=100&since=") != 1 {
			t.Fatal(f.calls)
		}
	}
}

func TestGistNotFoundRecreatedOnce(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.fail["gist create"] = true
	Submit(context.Background(), f.exec, dir, draft)
	p, _ := Pending(dir)
	f.fail["gist create"] = false
	if _, err := Resume(context.Background(), f.exec, dir, p[0].ReportID); err != nil {
		t.Fatal(err)
	}
	if f.count("gist create") != 2 || f.count("issue create") != 1 || f.count("api --paginate repos") != 0 {
		t.Fatal(f.calls)
	}
}

func TestConfirm(t *testing.T) {
	f := newFake()
	r, err := Submit(context.Background(), f.exec, t.TempDir(), Draft{Mode: Confirm, Repo: "projectbluefin/bluefin", Body: "same here", IssueRef: "#3"})
	if err != nil || f.count("gist") != 0 || f.count("issue comment --repo projectbluefin/bluefin --body-file - -- 3") != 1 || f.count("issue create") != 0 || r.IssueURL == "" {
		t.Fatal(f.calls, r, err)
	}
}

func TestConfirmUncertainFoundComment(t *testing.T) {
	f, dir := newFake(), t.TempDir()
	f.fail["issue comment"] = true
	d := Draft{Mode: Confirm, Repo: "projectbluefin/bluefin", Body: "x", IssueRef: "https://github.com/projectbluefin/bluefin/issues/3"}
	Submit(context.Background(), f.exec, dir, d)
	p, _ := Pending(dir)
	f.out["api comments"] = "https://github.com/projectbluefin/bluefin/issues/3#issuecomment-5\n"
	r, err := Resume(context.Background(), f.exec, dir, p[0].ReportID)
	if err != nil || f.count("issue comment") != 1 || !strings.HasSuffix(r.IssueURL, "-5") || f.count("api --paginate repos/projectbluefin/bluefin/issues/3/comments?") != 1 {
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
	f.fail["gist create"] = true
	Submit(context.Background(), f.exec, dir, draft)
	p, _ := Pending(dir)
	f = newFake()
	f.fail["auth status"] = true
	if _, err := Resume(context.Background(), f.exec, dir, p[0].ReportID); !errors.Is(err, ErrNotSignedIn) || len(f.calls) != 1 {
		t.Fatal(err, f.calls)
	}
}

func TestIssueRef(t *testing.T) {
	const repo = "projectbluefin/bluefin"
	for _, ok := range []string{"12", "#12", "https://github.com/projectbluefin/bluefin/issues/12", "https://github.com/ProjectBluefin/Bluefin/issues/12#issuecomment-1"} {
		if n, err := ParseIssueRef(repo, ok); n != 12 || err != nil {
			t.Error(ok, n, err)
		}
	}
	for _, bad := range []string{"--web", "-1", "0", "#", "", "abc", "12 --web", "https://github.com/evil/repo/issues/12", "https://github.com/projectbluefin/bluefin-lts/issues/12", "https://github.com/projectbluefin/bluefin/pull/12"} {
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
	f.fail["gist create"] = true
	Submit(context.Background(), f.exec, dir, draft)
	p, _ := Pending(dir)
	if err := Discard(dir, p[0].ReportID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, p[0].ReportID)); !os.IsNotExist(err) || Discard(dir, "..") == nil {
		t.Fatal("not removed", err)
	}
}
