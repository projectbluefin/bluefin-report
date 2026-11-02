package ui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/projectbluefin/bluefin-report/internal/publish"
)

// ---------- Publish ----------

func (u *ui) publish() {
	if u.publishing {
		return
	}
	d := publish.Draft{
		ReportID: u.reportID,
		Mode:     u.mode,
		Repo:     u.repoName,
		Body:     bufText(u.issueText),
	}
	if d.Mode == publish.NewIssue {
		d.Title = strings.TrimSpace(u.title.GetText())
		d.Queue = queues[u.queue.GetSelected()]
		d.Area = areas[u.area.GetSelected()]
	} else {
		d.IssueRef = strings.TrimSpace(u.issueRef.GetText())
	}
	if u.attach.GetActive() {
		d.Attachment = u.payload.JSON
	}
	d.Screenshots = slices.Clone(u.shots)
	log.Printf("ui: publishing %s to %s (mode %d, %d attachment bytes, %d screenshots)", d.ReportID, d.Repo, d.Mode, len(d.Attachment), len(d.Screenshots))
	u.run(d.ReportID, func() { u.publish() }, func(ctx context.Context, gh publish.Exec) (publish.Result, error) {
		return publish.Submit(ctx, gh, u.stateDir, d)
	})
}

func (u *ui) resume(id string) {
	if u.publishing {
		return
	}
	if s, ok := u.unfinished(id); ok {
		u.restoreCompose(s.Draft, id)
		return
	}
	log.Printf("ui: resuming %s", id)
	u.run(id, func() { u.resume(id) }, func(ctx context.Context, gh publish.Exec) (publish.Result, error) {
		return publish.Resume(ctx, gh, u.stateDir, id)
	})
}

// run does one publish step for report id off the main thread; retry repeats it after sign-in.
func (u *ui) run(id string, retry func(), step func(context.Context, publish.Exec) (publish.Result, error)) {
	gh := publish.Exec(publish.RunGH)
	if u.dryRun {
		gh = fakeGH
	}
	u.publishing = true
	u.syncPublish()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		res, err := step(ctx, gh)
		post(func() {
			u.publishing = false
			u.syncPublish()
			log.Printf("ui: publish finished: issue=%q err=%v", res.IssueURL, err)
			switch {
			case errors.Is(err, publish.ErrNotSignedIn):
				u.retry = retry
				u.signInDlg.Present(&u.win.Widget)
			case errors.Is(err, publish.ErrDraftChanged):
				u.refreshDrafts()
				u.showError("Couldn't Publish Report", err.Error())
			case err != nil:
				u.refreshDrafts()
				body := err.Error()
				if u.resumable(id) {
					body += "\n\nYour draft was kept. Resume it from Drafts."
				}
				u.showError("Couldn't Publish Report", body)
			default:
				u.refreshDrafts()
				u.doneURL = res.IssueURL
				u.donePage.SetDescription(res.IssueURL)
				if id == u.reportID { // the Compose report, not some other resumed draft
					u.discardSavedCompose()
					u.resetCompose()
				}
				u.nav.ReplaceWithTags([]string{"overview", "done"}, 2) // Back from Done goes home, not to Publish
			}
		})
	}()
}

// resumable reports whether Drafts holds report id in a form Resume can still publish.
func (u *ui) resumable(id string) bool {
	ds, _ := publish.Pending(u.stateDir)
	for _, s := range ds {
		if s.ReportID == id {
			return slices.Contains(publish.Repos, s.Draft.Repo)
		}
	}
	return false
}

// ---------- Unfinished reports ----------

// composeForm is what the Compose page holds.
type composeForm struct {
	Mode                                            publish.Mode
	Repo, Title, IssueRef, Desc, Steps, Queue, Area string
	Shots                                           []string
}

// composeDraft is the Compose form as an Unfinished draft; ok is false when there is nothing worth keeping.
func composeDraft(id string, f composeForm) (d publish.Draft, ok bool) {
	d = publish.Draft{
		ReportID: id,
		Mode:     f.Mode,
		Repo:     f.Repo,
		Body:     f.Desc,
		Steps:    f.Steps,
		IssueRef: strings.TrimSpace(f.IssueRef),
	}
	if f.Mode == publish.NewIssue {
		d.Title, d.Queue, d.Area = strings.TrimSpace(f.Title), f.Queue, f.Area
		d.IssueRef = ""
	}
	d.Screenshots = slices.Clone(f.Shots)
	ok = strings.TrimSpace(d.Title) != "" || strings.TrimSpace(f.Desc) != "" || strings.TrimSpace(f.Steps) != "" || len(f.Shots) > 0
	return d, ok
}

// draftForm is the Compose form an Unfinished draft reopens into.
func draftForm(d publish.Draft) composeForm {
	return composeForm{
		Mode: d.Mode, Repo: d.Repo, Title: d.Title, IssueRef: d.IssueRef, Desc: d.Body, Steps: d.Steps,
		Queue: d.Queue, Area: d.Area, Shots: slices.Clone(d.Screenshots),
	}
}

// saveCompose keeps the unsent Compose form as an Unfinished draft. Dry runs never touch drafts.
func (u *ui) saveCompose(explicit bool) {
	if u.dryRun {
		if explicit {
			u.toast("Drafts are not saved in a dry run")
		}
		return
	}
	if u.publishing { // the report is already being written under its own ID
		return
	}
	id := u.draftID
	if id == "" {
		id = publish.NewReportID()
	}
	d, ok := composeDraft(id, composeForm{
		Mode: u.mode, Repo: u.repoName, Title: u.title.GetText(), IssueRef: u.issueRef.GetText(),
		Desc: bufText(u.desc), Steps: bufText(u.steps),
		Queue: queues[u.queue.GetSelected()], Area: areas[u.area.GetSelected()], Shots: u.shots,
	})
	if !ok {
		if explicit {
			u.toast("Nothing to save yet")
		}
		return
	}
	shots, err := publish.SaveUnfinished(u.stateDir, d)
	if err != nil {
		log.Print("ui: save draft: ", err)
		if explicit {
			u.showError("Couldn't Save Draft", err.Error())
		}
		return
	}
	u.draftID = id
	u.shots = shots // the draft's copies outlive the originals and any earlier draft they came from
	u.syncShots()
	log.Printf("ui: saved unfinished report %s", id)
	if explicit {
		u.refreshDrafts()
		u.toast("Draft saved")
	}
}

// discardSavedCompose deletes the Unfinished draft of a Compose form that has just been published.
func (u *ui) discardSavedCompose() {
	if u.draftID == "" {
		return
	}
	if err := publish.Discard(u.stateDir, u.draftID); err != nil {
		log.Print("ui: discard saved draft: ", err)
	}
}

// detachCompose unlinks Compose from draft id being discarded, so a later save makes a new draft.
func (u *ui) detachCompose(id string) {
	if id != "" && id == u.draftID {
		u.draftID, u.detachedDraft = "", id
	}
}

// dropDraftShots removes Compose screenshots that were copies inside deleted draft id.
func (u *ui) dropDraftShots(id string) {
	n := len(u.shots)
	u.shots = slices.DeleteFunc(u.shots, func(p string) bool {
		return strings.HasPrefix(p, filepath.Join(u.stateDir, id)+string(filepath.Separator))
	})
	if len(u.shots) != n {
		u.syncShots()
	}
}

// unfinished returns the saved Unfinished draft id, if there is one.
func (u *ui) unfinished(id string) (publish.Saved, bool) {
	ds, _ := publish.Pending(u.stateDir)
	for _, s := range ds {
		if s.ReportID == id && s.Stage == publish.Unfinished {
			return s, true
		}
	}
	return publish.Saved{}, false
}

// restoreCompose loads an Unfinished draft into Compose, first keeping whatever is being written there.
func (u *ui) restoreCompose(d publish.Draft, id string) {
	if u.draftID != id {
		u.saveCompose(false)
	}
	f := draftForm(d)
	u.resetCompose()
	u.mode = f.Mode
	u.title.SetText(f.Title)
	u.issueRef.SetText(f.IssueRef)
	u.desc.SetText(f.Desc, -1)
	u.steps.SetText(f.Steps, -1)
	if i := slices.Index(queues, f.Queue); i >= 0 {
		u.queue.SetSelected(uint32(i))
	}
	if i := slices.Index(areas, f.Area); i >= 0 {
		u.area.SetSelected(uint32(i))
	}
	u.shots = f.Shots
	u.syncShots()
	u.draftID = id
	log.Printf("ui: reopened unfinished report %s", id)
	u.openCompose(f.Mode)
}

// ---------- Drafts ----------

func (u *ui) buildDrafts() {
	u.draftList = gtk.NewListBox()
	u.draftList.SetSelectionMode(gtk.SelectionNoneValue)
	u.draftList.AddCssClass("boxed-list")
	g := group("Unfinished Reports")
	g.SetDescription("Discarding only deletes the local copy, never anything already on GitHub")
	g.Add(&u.draftList.Widget)
	empty := adw.NewStatusPage()
	empty.SetIconName("document-edit-symbolic")
	empty.SetTitle("No Unfinished Reports")
	empty.SetDescription("Reports that couldn't be published appear here")
	u.draftsStack = gtk.NewStack()
	u.draftsStack.AddNamed(scrolled(&g.Widget), "list")
	u.draftsStack.AddNamed(&empty.Widget, "empty")
	u.nav.Add(u.page("Drafts", "drafts", nil, &u.draftsStack.Widget))

	// Discard is undoable: the draft leaves the list at once and its files go
	// only when the toast is dismissed without Undo (or the window closes).
	u.discarded = map[uintptr]string{}
	u.onUndo = hold(func(t adw.Toast) {
		log.Printf("ui: discard of draft %s undone", u.discarded[t.Ptr])
		id := u.discarded[t.Ptr]
		delete(u.discarded, t.Ptr) // button-clicked precedes dismissed, which then finds nothing to delete
		if id == u.detachedDraft && u.draftID == "" {
			u.draftID = id // Compose saves to its draft again
		}
		u.detachedDraft = ""
		u.refreshDrafts()
	})
	u.onDismissed = hold(func(t adw.Toast) {
		if u.undoToast != nil && u.undoToast.Ptr == t.Ptr {
			u.undoToast = nil
		}
		id, ok := u.discarded[t.Ptr]
		if !ok {
			return
		}
		delete(u.discarded, t.Ptr)
		if u.publishing { // the draft may be the report now publishing; keep it
			log.Printf("ui: keeping draft %s: publishing", id)
		} else {
			log.Printf("ui: deleting draft %s", id)
			if err := publish.Discard(u.stateDir, id); err != nil {
				u.showError("Couldn't Discard Draft", err.Error())
			}
			u.dropDraftShots(id)
		}
		if id == u.detachedDraft {
			u.detachedDraft = ""
		}
		u.refreshDrafts()
	})
}

// refreshDrafts rebuilds the Drafts list and the Overview row from disk,
// leaving out drafts whose discard can still be undone.
func (u *ui) refreshDrafts() {
	ds, err := publish.Pending(u.stateDir)
	if err != nil {
		log.Print("ui: drafts: ", err)
	}
	ds = slices.DeleteFunc(ds, func(s publish.Saved) bool {
		for _, id := range u.discarded {
			if id == s.ReportID {
				return true
			}
		}
		return false
	})
	log.Printf("ui: %d unfinished drafts", len(ds))
	u.draftsCount.SetText(fmt.Sprint(len(ds)))
	u.draftsGrp.SetVisible(len(ds) > 0)
	u.draftList.RemoveAll()
	for _, s := range ds {
		r := adw.NewActionRow()
		t := s.Draft.Title
		if t == "" && s.Stage == publish.Unfinished && s.Draft.Mode == publish.NewIssue {
			t = "Untitled report"
		} else if t == "" {
			t = "Comment on #" + s.Draft.IssueRef
			if s.Draft.Mode == publish.Update {
				t = "Update on #" + s.Draft.IssueRef
			}
		}
		r.SetTitle(t)
		r.SetSubtitle(s.Draft.Repo + " · " + stageText(s))
		resume := gtk.NewButtonWithLabel("Resume")
		resume.SetValign(gtk.AlignCenterValue)
		resume.AddCssClass("flat")
		resume.SetActionName("win.draft-resume")
		resume.SetActionTargetValue(glib.NewVariantString(s.ReportID))
		r.AddSuffix(&resume.Widget)
		discard := gtk.NewButtonFromIconName("user-trash-symbolic")
		discard.SetTooltipText("Discard")
		discard.SetValign(gtk.AlignCenterValue)
		discard.AddCssClass("flat")
		discard.SetActionName("win.draft-discard")
		discard.SetActionTargetValue(glib.NewVariantString(s.ReportID))
		r.AddSuffix(&discard.Widget)
		u.draftList.Append(&r.Widget)
	}
	if err != nil { // the readable drafts are still listed above
		r := adw.NewActionRow()
		r.SetTitle("Some saved reports couldn't be read")
		r.SetSubtitle(err.Error())
		r.SetSubtitleSelectable(true)
		r.AddPrefix(&gtk.NewImageFromIconName("dialog-warning-symbolic").Widget)
		u.draftList.Append(&r.Widget)
	}
	if len(ds) == 0 && err == nil {
		u.draftsStack.SetVisibleChildName("empty")
	} else {
		u.draftsStack.SetVisibleChildName("list")
	}
}

// stageText describes a draft's publish stage in plain words.
func stageText(s publish.Saved) string {
	switch {
	case s.Stage == publish.Unfinished:
		return "Not finished writing"
	case s.Stage == publish.Reviewed:
		return "Not published yet"
	case s.IssueURL != "":
		return "Partly published"
	}
	return "Interrupted while publishing"
}

// ---------- Sign-in ----------

func (u *ui) buildSignIn() {
	u.signInDlg = adw.NewAlertDialog("Sign In to GitHub",
		"Publishing needs a GitHub account. Sign in with a one-time code in your browser; your draft is kept.")
	u.signInDlg.Ref()
	u.signInDlg.AddResponse("cancel", "_Cancel")
	u.signInDlg.AddResponse("signin", "_Sign In")
	u.signInDlg.SetDefaultResponse("signin")
	u.signInDlg.SetResponseAppearance("signin", adw.ResponseSuggestedValue)
	u.signInDlg.SetCloseResponse("cancel")
	u.signInDlg.ConnectResponse(hold(func(_ adw.AlertDialog, r string) {
		if r == "signin" {
			u.signIn()
		}
	}))

	u.codeLabel = gtk.NewLabel("…")
	u.codeLabel.AddCssClass("title-1")
	u.codeLabel.SetSelectable(true)
	cp := gtk.NewButtonFromIconName("edit-copy-symbolic")
	cp.SetTooltipText("Copy Code")
	cp.SetValign(gtk.AlignCenterValue)
	cp.AddCssClass("flat")
	cp.ConnectClicked(hold(func(gtk.Button) {
		u.win.GetClipboard().SetText(u.codeLabel.GetText())
		u.toast("Code copied")
	}))
	code := gtk.NewBox(gtk.OrientationHorizontalValue, 6)
	code.SetHalign(gtk.AlignCenterValue)
	code.Append(&u.codeLabel.Widget)
	code.Append(&cp.Widget)
	open := pill("Open Browser", func() {
		gtk.NewUriLauncher(u.codeURL).Launch(&u.win.Window, nil, nil, 0)
	})
	open.AddCssClass("suggested-action")
	open.SetHalign(gtk.AlignCenterValue)
	box := gtk.NewBox(gtk.OrientationVerticalValue, 12)
	box.Append(&code.Widget)
	box.Append(&open.Widget)

	u.codeDlg = adw.NewAlertDialog("Enter this code on GitHub", "Copy the code, open the browser, and paste it there. This closes by itself once GitHub confirms.")
	u.codeDlg.Ref()
	u.codeDlg.SetExtraChild(&box.Widget)
	u.codeDlg.AddResponse("cancel", "_Cancel")
	u.codeDlg.SetCloseResponse("cancel")
	u.codeDlg.ConnectResponse(hold(func(adw.AlertDialog, string) {
		if u.signCancel != nil {
			u.signCancel()
		}
	}))
}

func (u *ui) signIn() {
	gh := publish.StreamExec(publish.RunGHStream)
	if u.dryRun {
		gh = fakeGHStream
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	u.signCancel = cancel
	u.codeLabel.SetText("…")
	u.codeURL = "https://github.com/login/device"
	u.codeDlg.Present(&u.win.Widget)
	log.Print("ui: signing in to GitHub")
	go func() {
		defer cancel()
		err := publish.SignIn(ctx, gh, func(code, url string) {
			post(func() {
				log.Printf("ui: sign-in code received (%s)", url)
				u.codeLabel.SetText(code)
				u.codeURL = url
			})
		})
		post(func() {
			u.signCancel = nil
			u.codeDlg.ForceClose()
			log.Printf("ui: sign-in finished: err=%v", err)
			if err != nil {
				if ctx.Err() == nil { // not cancelled by the user
					u.showError("Couldn't Sign In", err.Error())
				}
				return
			}
			u.toast("Signed in to GitHub")
			if u.retry != nil {
				u.retry()
			}
		})
	}()
}

// ---------- Done ----------

func (u *ui) buildDone() {
	u.donePage = adw.NewStatusPage()
	u.donePage.SetIconName("object-select-symbolic") // emblem-ok-symbolic is gone from the Adwaita icon theme
	u.donePage.SetTitle("Report published")
	open := pill("Open Issue", func() {
		gtk.NewUriLauncher(u.doneURL).Launch(&u.win.Window, nil, nil, 0)
	})
	open.AddCssClass("suggested-action")
	cp := pill("Copy Link", func() {
		u.win.GetClipboard().SetText(u.doneURL)
		u.toast("Link copied")
	})
	btns := gtk.NewBox(gtk.OrientationHorizontalValue, 12)
	btns.SetHomogeneous(true) // HIG: adjacent buttons share a width
	btns.SetHalign(gtk.AlignCenterValue)
	btns.Append(&open.Widget)
	btns.Append(&cp.Widget)
	u.donePage.SetChild(&btns.Widget)
	u.nav.Add(u.page("Done", "done", nil, &u.donePage.Widget))
}
