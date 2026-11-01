package publish

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

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
	if len(d.Screenshots) > 0 && d.Mode == NewIssue && !s.ShotsArtifact {
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
		var artifacts []string
		if (d.Attachment != nil || len(d.Screenshots) > 0) && !fresh {
			out, errb, err := gh(ctx, nil, "issue", "artifact", "list", n, "-R", d.Repo, "--json", "name", "--jq", ".[].name")
			if err != nil && !artifactsRefused(errb) {
				return warn("reading artifacts", err, errb)
			}
			refused = err != nil
			artifacts = strings.Split(strings.TrimSpace(string(out)), "\n")
			exists = slices.Contains(artifacts, name)
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
		if len(d.Screenshots) > 0 && !refused {
			// One Markdown artifact; gh appends every unreferenced --attach image to it. A partial
			// upload still saves the artifact, so a retry finds it by name and does not upload again.
			shots := shotsArtifactName(d)
			if !slices.Contains(artifacts, shots) {
				args := append([]string{"issue", "artifact", "create", n, "--body-file", "-", "--name", shots, "-R", d.Repo}, attachArgs(d.Screenshots)...)
				if _, errb, err := gh(ctx, []byte("Screenshots\n"), args...); err != nil {
					if !artifactsRefused(errb) {
						return warn("uploading screenshots", err, errb)
					}
				} else {
					s.ShotsArtifact = true
				}
			} else {
				s.ShotsArtifact = true
			}
			if s.ShotsArtifact && final == ShotsPending {
				final = Published
			}
		}
		s.Stage, freshShots = final, true
		if refused && d.Attachment != nil {
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
			args := []string{"issue", "comment", "--repo", d.Repo, "--body-file", "-"}
			if !s.ShotsArtifact {
				args = append(args, attachArgs(d.Screenshots)...)
			}
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
