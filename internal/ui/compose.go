package ui

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"

	"github.com/projectbluefin/bluefin-report/internal/extras"
	"github.com/projectbluefin/bluefin-report/internal/privacy"
	"github.com/projectbluefin/bluefin-report/internal/publish"
	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

// ---------- Compose ----------

func (u *ui) buildCompose() {
	g := adw.NewPreferencesGroup()
	u.repo = adw.NewActionRow()
	u.repo.SetTitle("Project")
	u.repo.AddCssClass("property")
	u.repo.SetSubtitleSelectable(true)
	g.Add(&u.repo.Widget)
	sync := hold(func(gobject.Object, uintptr) { u.syncReview() })

	u.title = adw.NewEntryRow()
	u.title.SetTitle("Title")
	u.title.ConnectNotify(sync)
	g.Add(&u.title.Widget)

	u.issueRef = adw.NewEntryRow()
	u.issueRef.SetTitle("Issue link or number (#123)")
	u.issueRef.ConnectNotify(sync)
	g.Add(&u.issueRef.Widget)
	u.refErr = gtk.NewLabel("")
	u.refErr.SetXalign(0)
	u.refErr.SetWrap(true)
	u.refErr.SetMarginTop(6)
	u.refErr.AddCssClass("error")
	u.refErr.AddCssClass("caption")
	u.refErr.SetVisible(false)
	g.Add(&u.refErr.Widget) // not a row: AdwPreferencesGroup places it below the list

	dg := group("What Happened?")
	u.descGroup = dg
	u.desc = gtk.NewTextBuffer(nil)
	tv := gtk.NewTextViewWithBuffer(u.desc)
	u.descView = tv
	tv.SetWrapMode(gtk.WrapWordCharValue)
	tv.SetTopMargin(12)
	tv.SetBottomMargin(12)
	tv.SetLeftMargin(12)
	tv.SetRightMargin(12)
	tv.SetSizeRequest(-1, 160)
	tv.AddCssClass("card")
	tv.SetAcceptsTab(false) // Tab moves focus, as in every other form field
	dg.Add(&tv.Widget)

	stepsGroup := group("Steps to Reproduce (Optional)")
	u.steps = gtk.NewTextBuffer(nil)
	stv := gtk.NewTextViewWithBuffer(u.steps)
	stv.SetWrapMode(gtk.WrapWordCharValue)
	stv.SetTopMargin(12)
	stv.SetBottomMargin(12)
	stv.SetLeftMargin(12)
	stv.SetRightMargin(12)
	stv.SetSizeRequest(-1, 120)
	stv.AddCssClass("card")
	stv.SetAcceptsTab(false)
	a11yLabel(&stv.Widget, "Steps to reproduce")
	stepsGroup.Add(&stv.Widget)

	og := group("Options")
	u.attach = adw.NewSwitchRow()
	u.attach.SetTitle("Attach System Report")
	u.attach.SetSubtitle("Reviewed before anything is sent")
	u.attach.SetActive(true)
	og.Add(&u.attach.Widget)
	u.includeUser = adw.NewSwitchRow()
	u.includeUser.SetTitle("Include User Session")
	og.Add(&u.includeUser.Widget)
	u.queue = adw.NewComboRow()
	u.queue.SetTitle("Who should look at it")
	u.queue.SetModel(gtk.NewStringList([]string{"No preference", "Machine analysis", "Human only"}))
	og.Add(&u.queue.Widget)
	u.area = adw.NewComboRow()
	u.area.SetTitle("Area")
	u.area.SetModel(gtk.NewStringList([]string{"Not sure", "Desktop", "Flatpak apps", "Gaming", "Hardware", "Installer", "Developer tools"}))
	og.Add(&u.area.Widget)

	xg := group("Include Diagnostics")
	xg.SetDescription("Off by default. Scrubbed like the report and shown in Review before anything is sent")
	for _, s := range extras.Sections {
		r := adw.NewSwitchRow()
		r.SetTitle(s.Title)
		xg.Add(&r.Widget)
		u.optIn = append(u.optIn, r)
	}

	sg := group("Screenshots")
	sg.SetDescription("Public. Metadata is stripped by re-encoding. PNG, JPEG or GIF; up to 10 files, 10 MB each")
	u.shotList = gtk.NewListBox()
	u.shotList.SetSelectionMode(gtk.SelectionNoneValue)
	u.shotList.AddCssClass("boxed-list")
	sg.Add(&u.shotList.Widget)
	filter := gtk.NewFileFilter()
	filter.SetName("Images")
	for _, ext := range publish.ScreenshotExts {
		filter.AddSuffix(ext[1:])
	}
	u.shotDialog = gtk.NewFileDialog()
	u.shotDialog.SetTitle("Add Screenshots")
	u.shotDialog.SetDefaultFilter(filter)
	picked := hold(gio.AsyncReadyCallback(func(_, res, _ uintptr) {
		files, err := u.shotDialog.OpenMultipleFinish(gio.SimpleAsyncResultNewFromInternalPtr(res))
		if err != nil || files == nil {
			return // cancelled
		}
		for i := range files.GetNItems() {
			o := files.GetObject(i)
			if p := (&gio.FileBase{Ptr: o.Ptr}).GetPath(); p != "" && !slices.Contains(u.shots, p) && fileExists(p) {
				u.shots = append(u.shots, p)
			}
			o.Unref()
		}
		if len(u.shots) > publish.MaxScreenshots {
			u.shots = u.shots[:publish.MaxScreenshots]
			u.toast(fmt.Sprintf("Only %d screenshots can be attached", publish.MaxScreenshots))
		}
		u.syncShots()
	}))
	u.addShot = adw.NewButtonRow() // HIG: add button row at the end of an editable list
	u.addShot.Ref()                // syncShots re-appends it after RemoveAll
	u.addShot.SetTitle("Add Screenshots…")
	u.addShot.SetStartIconName("list-add-symbolic")
	u.addShot.ConnectActivated(hold(func(adw.ButtonRow) { u.shotDialog.OpenMultiple(&u.win.Window, nil, picked, 0) }))
	u.shotList.Append(&u.addShot.Widget)

	u.reviewBtn = pill("Review", u.openReview)
	u.reviewBtn.AddCssClass("suggested-action")
	u.reviewBtn.SetHalign(gtk.AlignCenterValue)
	u.reviewBtn.SetMarginTop(6)
	u.reviewBtn.SetMarginBottom(6)
	u.reviewBtn.SetSensitive(false)

	u.newOnly = []*gtk.Widget{&u.title.Widget, &u.queue.Widget, &u.area.Widget, &stepsGroup.Widget}
	tv2 := u.toolbar(nil, scrolled(&g.Widget, &dg.Widget, &stepsGroup.Widget, &og.Widget, &xg.Widget, &sg.Widget))
	tv2.AddBottomBar(&u.reviewBtn.Widget) // like Review's Publish: always visible, never scrolled away
	u.composePage = adw.NewNavigationPage(&tv2.Widget, "New Issue")
	u.composePage.SetTag("compose")
	u.nav.Add(u.composePage)
}

// syncShots rebuilds the screenshot list; each Remove button targets win.shot-remove with its path.
func (u *ui) syncShots() {
	u.shotList.RemoveAll()
	for _, p := range u.shots {
		r := adw.NewActionRow()
		r.SetTitle(filepath.Base(p))
		r.SetTitleLines(1)
		b := gtk.NewButtonFromIconName("list-remove-symbolic")
		b.SetTooltipText("Remove")
		b.SetValign(gtk.AlignCenterValue)
		b.AddCssClass("flat")
		b.SetActionName("win.shot-remove")
		b.SetActionTargetValue(glib.NewVariantString(p))
		r.AddSuffix(&b.Widget)
		u.shotList.Append(&r.Widget)
	}
	u.shotList.Append(&u.addShot.Widget)
	if u.consentIssue != nil { // shots changed: Review consent no longer covers them
		u.consentIssue.SetActive(false)
		u.consentReport.SetActive(false)
	}
}

// syncReview enables Review once the snapshot is ready and the mode's
// required field is filled.
func (u *ui) syncReview() {
	if u.reviewBtn == nil {
		return // notify fires while the page is still being built
	}
	ok := strings.TrimSpace(u.title.GetText()) != ""
	msg := ""
	if u.mode != publish.NewIssue {
		ref := strings.TrimSpace(u.issueRef.GetText())
		_, err := publish.ParseIssueRef(u.repoName, ref)
		ok = err == nil
		if err != nil && ref != "" {
			msg = "Enter an issue number or a link to an issue in " + u.repoName
		}
	}
	u.refErr.SetText(msg)
	u.refErr.SetVisible(msg != "")
	if msg != "" {
		u.issueRef.AddCssClass("error")
	} else {
		u.issueRef.RemoveCssClass("error")
	}
	u.reviewBtn.SetSensitive(u.snap != nil && ok)
}

// openCompose shows the form for m. A half-written report survives Back or
// Escape; only switching modes (or publishing it) starts a fresh one.
func (u *ui) openCompose(m publish.Mode) {
	if m != u.mode {
		u.resetCompose()
	}
	u.mode = m
	newIssue := m == publish.NewIssue
	for _, w := range u.newOnly {
		w.SetVisible(newIssue)
	}
	u.issueRef.SetVisible(!newIssue)
	desc, page := "What Happened?", "New Issue"
	switch m {
	case publish.Confirm:
		desc, page = "What You’re Seeing (Optional)", "Confirm Existing Issue"
	case publish.Update:
		desc, page = "What Changed? (Optional)", "Update Your Issue"
	}
	u.descGroup.SetTitle(desc)
	a11yLabel(&u.descView.Widget, desc)
	u.composePage.SetTitle(page)
	u.repo.SetSubtitle(u.repoName)
	u.syncReview()
	u.nav.PushByTag("compose")
}

// resetCompose empties the Compose form to its defaults.
func (u *ui) resetCompose() {
	u.draftID = ""
	u.title.SetText("")
	u.issueRef.SetText("")
	u.desc.SetText("", -1)
	u.steps.SetText("", -1)
	u.attach.SetActive(true)
	u.includeUser.SetActive(false)
	u.queue.SetSelected(0)
	u.area.SetSelected(0)
	for _, r := range u.optIn {
		r.SetActive(false)
	}
	u.shots = nil
	u.syncShots()
}

// ---------- Review ----------

func monoView(buf *gtk.TextBuffer, editable bool, label string) *gtk.Widget {
	tv := gtk.NewTextViewWithBuffer(buf)
	tv.SetMonospace(true)
	tv.SetEditable(editable)
	tv.SetAcceptsTab(false) // Tab moves focus instead of inserting a tab
	tv.SetWrapMode(gtk.WrapWordCharValue)
	tv.SetTopMargin(12)
	tv.SetBottomMargin(12)
	tv.SetLeftMargin(12)
	tv.SetRightMargin(12)
	a11yLabel(&tv.Widget, label)
	sw := gtk.NewScrolledWindow()
	sw.SetChild(&tv.Widget)
	sw.SetVexpand(true)
	return &sw.Widget
}

func (u *ui) buildReview() {
	u.issueText = gtk.NewTextBuffer(nil)
	u.issueText.ConnectChanged(hold(func(gtk.TextBuffer) {
		if !u.settingText {
			u.consentIssue.SetActive(false) // edited text needs fresh consent (§6.4)
			u.consentReport.SetActive(false)
		}
	}))
	u.attachText = gtk.NewTextBuffer(nil)
	u.removed = gtk.NewListBox()
	u.removed.SetSelectionMode(gtk.SelectionNoneValue)
	u.removed.AddCssClass("boxed-list")

	stack := adw.NewViewStack()
	stack.AddTitledWithIcon(monoView(u.issueText, true, "Issue text"), "issue", "Issue Text", "document-edit-symbolic")
	stack.AddTitledWithIcon(monoView(u.attachText, false, "Attachment"), "attachment", "Attachment", "mail-attachment-symbolic")
	rg := group("What Was Removed")
	rg.SetDescription("Values scrubbed or dropped before sharing")
	rg.Add(&u.removed.Widget)
	rsw := gtk.NewScrolledWindow() // not scrolled(): the page is already clamped and padded
	rsw.SetChild(&rg.Widget)
	stack.AddTitledWithIcon(&rsw.Widget, "removed", "Removed", "edit-clear-symbolic")
	stack.SetSizeRequest(-1, 280) // outer scroll keeps Consent reachable at 360×294

	// Inline, not in the header: it switches only this panel, and the header keeps the page title.
	sw := adw.NewInlineViewSwitcher()
	sw.SetStack(stack)
	sw.SetHalign(gtk.AlignCenterValue)

	u.shotsGroup = group("Attachments")
	u.consentGroup = group("Consent")
	cg := u.consentGroup
	u.consentIssue = adw.NewSwitchRow()
	u.consentIssue.SetTitle("I understand the issue text will be public")
	u.consentReport = adw.NewSwitchRow()
	u.consentReport.SetTitle("I understand the full report will be attached publicly to the issue")
	u.consentReport.SetSubtitle("Anyone who can see the issue can read it")
	sync := hold(func(gobject.Object, uintptr) { u.syncPublish() })
	u.consentIssue.ConnectNotify(sync)
	u.consentReport.ConnectNotify(sync)
	cg.Add(&u.consentIssue.Widget)
	cg.Add(&u.consentReport.Widget)

	u.saveDialog = gtk.NewFileDialog()
	u.saveDialog.SetTitle("Export Report")
	saved := hold(gio.AsyncReadyCallback(func(_, res, _ uintptr) {
		f, err := u.saveDialog.SaveFinish(gio.SimpleAsyncResultNewFromInternalPtr(res))
		if err != nil || f == nil {
			return // cancelled
		}
		path := f.GetPath()
		if err := os.WriteFile(path, u.payload.JSON, 0o600); err != nil {
			u.showError("Couldn't Export Report", err.Error())
			return
		}
		log.Printf("ui: exported %d bytes to %s", len(u.payload.JSON), path)
		u.toast("Report exported")
	}))
	export := pill("Export Report…", func() {
		u.saveDialog.SetInitialName("bluefin-report.json")
		u.saveDialog.Save(&u.win.Window, nil, saved, 0)
	})
	u.publishBtn = pill("Publish", u.publish)
	u.publishBtn.AddCssClass("suggested-action")
	btns := gtk.NewBox(gtk.OrientationHorizontalValue, 12) // not homogeneous: two Export-width pills overflow 360 px
	btns.SetHalign(gtk.AlignCenterValue)
	btns.Append(&export.Widget)
	btns.Append(&u.publishBtn.Widget)

	btns.SetMarginTop(6)
	btns.SetMarginBottom(6)

	tv := u.toolbar(nil, scrolled(&sw.Widget, &stack.Widget, &u.shotsGroup.Widget, &cg.Widget))
	tv.AddBottomBar(&btns.Widget)
	p := adw.NewNavigationPage(&tv.Widget, "Review")
	p.SetTag("review")
	u.nav.Add(p)
}

func (u *ui) syncPublish() {
	if u.publishBtn == nil {
		return // notify fires while the page is still being built
	}
	ok := !u.publishing && u.consentIssue.GetActive() && (!u.attach.GetActive() || u.consentReport.GetActive())
	u.publishBtn.SetSensitive(ok)
	if u.discardAct != nil {
		u.discardAct.SetEnabled(!u.publishing) // greys out every Discard button
	}
}

func bufText(b *gtk.TextBuffer) string {
	var s, e gtk.TextIter
	b.GetBounds(&s, &e)
	return b.GetText(&s, &e, false)
}

// openReview collects the opted-in diagnostics off the main thread, then shows Review.
// With none opted in, nothing is collected.
func (u *ui) openReview() {
	var keys []string
	for i, s := range extras.Sections {
		if u.optIn[i].GetActive() {
			keys = append(keys, s.Key)
		}
	}
	if len(keys) == 0 {
		u.showReview(nil)
		return
	}
	u.reviewBtn.SetSensitive(false)
	gen := u.gen
	go func() {
		vals := extras.Collect(context.Background(), keys)
		post(func() {
			u.syncReview()
			// A refresh during collection cleared or replaced the snapshot these belong to.
			if gen != u.gen || u.snap == nil {
				return
			}
			u.showReview(vals)
		})
	}()
}

func (u *ui) showReview(vals map[string]string) {
	name := ""
	if cur, err := user.Current(); err == nil {
		name = cur.Username
	}
	// Synchronous on purpose: measured ~40 ms (9.5k metrics, 725 KB payload) on a dev host.
	p, err := privacy.Build(*u.snap, privacy.Options{Username: name, IncludeUser: u.includeUser.GetActive(), Extras: vals})
	if err != nil {
		u.showError("Couldn't Prepare Report", err.Error())
		return
	}
	u.payload = p
	u.reportID = publish.NewReportID()
	log.Printf("ui: payload %d bytes, %d rules, %d omitted", len(p.JSON), len(p.Removed), len(p.Omitted))

	u.settingText = true
	u.issueText.SetText(u.markdown(), -1)
	u.settingText = false

	preview := string(p.JSON)
	if len(preview) > previewBytes {
		preview = strings.ToValidUTF8(preview[:previewBytes], "") +
			fmt.Sprintf("\n\n… preview truncated at 200 KB of %d KB. Export and Publish use the full report.\n", len(p.JSON)>>10)
	}
	if !u.attach.GetActive() {
		preview = "No attachment: “Attach system report” is off."
	}
	u.attachText.SetText(preview, -1)

	u.removed.RemoveAll()
	row := func(t, s string) {
		r := adw.NewActionRow()
		r.SetTitle(t)
		r.SetSubtitle(s)
		u.removed.Append(&r.Widget)
	}
	for rule, n := range p.Removed {
		row(rule, fmt.Sprintf("%d values", n))
	}
	for _, f := range p.Omitted {
		row(f, "Omitted to fit the size limit")
	}
	if len(p.Removed)+len(p.Omitted) == 0 {
		row("Nothing", "No values needed scrubbing")
	}
	names := make([]string, len(u.shots))
	for i, p := range u.shots {
		names[i] = filepath.Base(p)
	}
	u.shotsGroup.SetDescription(strings.Join(names, "\n"))
	u.shotsGroup.SetVisible(len(u.shots) > 0)
	u.consentGroup.SetDescription("")
	if len(u.shots) > 0 {
		n := "1 screenshot becomes"
		if len(u.shots) > 1 {
			n = fmt.Sprintf("%d screenshots become", len(u.shots))
		}
		u.consentGroup.SetDescription(n + " public. Metadata is stripped, but image content is not scrubbed.")
	}
	u.consentIssue.SetActive(false)
	u.consentReport.SetActive(false)
	u.consentReport.SetVisible(u.attach.GetActive())
	u.syncPublish()
	u.nav.PushByTag("review")
}

func (u *ui) markdown() string {
	return issueMarkdown(u.mode, bufText(u.desc), bufText(u.steps), u.sum, u.ident, u.payload.Extras)
}

// issueMarkdown is the issue or comment text Review pre-fills, before the
// publisher appends its metadata. It reads no widgets, so tests can pin it.
func issueMarkdown(mode publish.Mode, desc, steps string, sum snapshot.Summary, ident extras.Identity, details map[string]string) string {
	var b strings.Builder
	switch mode {
	case publish.Confirm:
		b.WriteString("Same problem here.\n\n")
	case publish.Update:
		b.WriteString("Updated report from the original reporter.\n\n")
	}
	if d := strings.TrimSpace(desc); d != "" {
		b.WriteString(d + "\n\n")
	}
	if s := strings.TrimSpace(steps); s != "" && mode == publish.NewIssue {
		b.WriteString("### Steps to reproduce\n\n" + s + "\n\n")
	}
	b.WriteString("| | |\n|---|---|\n")
	for _, kv := range [][2]string{
		{"System", sum.Name}, {"Version", sum.ImageVersion}, {"Image", sum.ImageID},
		{"Kernel", sum.Kernel}, {"Arch", sum.Arch}, {"State", sum.SystemState},
		{"Image ref", ident.Ref}, {"Tag", ident.Tag}, {"Digest", ident.Digest},
		{"CPU", ident.CPU}, {"GPU", ident.GPU}, {"GNOME", ident.GNOME},
		{"Failed units", fmt.Sprint(len(sum.FailedUnits))},
	} {
		fmt.Fprintf(&b, "| %s | %s |\n", kv[0], kv[1])
	}
	for _, f := range sum.FailedUnits {
		fmt.Fprintf(&b, "\n- `%s`", f)
	}
	b.WriteString(extras.Markdown(details))
	return b.String()
}
