// Package ui is the GTK4/Libadwaita front end of Bluefin Report.
//
// puregotk callbacks live in a fixed, never-freed table, so every signal is
// connected exactly once in build*; refreshes only mutate widgets and models.
package ui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"
	"codeberg.org/puregotk/puregotk/v4/pango"

	"github.com/projectbluefin/bluefin-report/internal/privacy"
	"github.com/projectbluefin/bluefin-report/internal/publish"
	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

const appID = "io.projectbluefin.Report"

// previewBytes caps the Attachment tab; exported/published bytes are always full.
const previewBytes = 200 << 10

var queues = []string{"", "3-clanker-queue", "3-human-queue"}

// Main-thread dispatch. puregotk caches callbacks by function-pointer address
// in a fixed, never-freed table, so a fresh closure per call would exhaust it;
// every post reuses the single package-level mainCb.
var (
	mainMu sync.Mutex
	mainQ  []func()
	mainCb glib.SourceFunc = func(uintptr) bool {
		mainMu.Lock()
		q := mainQ
		mainQ = nil
		mainMu.Unlock()
		for _, fn := range q {
			fn()
		}
		return false
	}
)

// post runs fn on the GLib main thread.
func post(fn func()) {
	mainMu.Lock()
	mainQ = append(mainQ, fn)
	mainMu.Unlock()
	glib.IdleAdd(&mainCb, 0)
}

type ui struct {
	dryRun   bool
	stateDir string
	app      *adw.Application
	win      *adw.ApplicationWindow
	toasts   *adw.ToastOverlay
	nav      *adw.NavigationView

	// overview
	gen        int
	cancel     context.CancelFunc
	refreshAct *gio.SimpleAction
	snap       *snapshot.Snapshot
	sum        snapshot.Summary
	ovStack    *gtk.Stack
	spinner    *adw.Spinner
	statusPage *adw.StatusPage
	sysRows    [6]*adw.ActionRow
	health     *gtk.ListBox

	// metrics
	metrics *gtk.StringList
	filter  *gtk.StringFilter
	search  *gtk.SearchEntry

	// compose
	mode        publish.Mode
	composePage *adw.NavigationPage
	repo        *adw.ActionRow // shows repoName; there is no choice: reports go to the running image's repo
	repoName    string
	helpGroup   *adw.PreferencesGroup
	helpBtns    *gtk.FlowBox
	title       *adw.EntryRow
	issueRef    *adw.EntryRow
	desc        *gtk.TextBuffer
	attach      *adw.SwitchRow
	includeUser *adw.SwitchRow
	queue       *adw.ComboRow
	newOnly     []*gtk.Widget
	reviewBtn   *gtk.Button
	refErr      *gtk.Label

	// review
	payload      privacy.Payload
	reportID     string
	publishing   bool
	issueText    *gtk.TextBuffer
	settingText  bool
	attachText   *gtk.TextBuffer
	removed      *gtk.ListBox
	consentIssue *adw.SwitchRow
	consentLink  *adw.SwitchRow
	publishBtn   *gtk.Button
	saveDialog   *gtk.FileDialog

	// done
	donePage *adw.StatusPage
	doneURL  string

	// drafts
	draftsRow  *adw.ActionRow
	draftList  *gtk.ListBox
	discardDlg *adw.AlertDialog
	discardID  string
	discardAct *gio.SimpleAction

	// sign-in
	signInDlg  *adw.AlertDialog
	codeDlg    *adw.AlertDialog
	codeLabel  *gtk.Label
	codeURL    string
	signCancel context.CancelFunc
	retry      func() // the publish/resume that hit ErrNotSignedIn
}

// keep holds every Go callback so the GC never collects one puregotk still points at.
var keep []any

func hold[T any](f T) *T { p := &f; keep = append(keep, p); return p }

// Run starts the GTK application and returns its exit code.
func Run(dryRun bool) int {
	u := &ui{dryRun: dryRun}
	u.app = adw.NewApplication(appID, gio.GApplicationNonUniqueValue)
	u.app.ConnectActivate(hold(func(gio.Application) { u.activate() }))
	return int(u.app.Run(1, []string{"bluefin-report"}))
}

func (u *ui) activate() {
	if u.win != nil {
		u.win.Present()
		return
	}
	log.Print("ui: building window")
	u.stateDir = stateDir()
	if u.dryRun {
		d, err := os.MkdirTemp("", "bluefin-report-dry-run-")
		if err != nil {
			log.Print("ui: ", err)
			u.app.Quit()
			return
		}
		u.stateDir = d
	}
	u.win = adw.NewApplicationWindow(&u.app.Application)
	u.win.SetDefaultSize(720, 760)
	u.win.SetSizeRequest(360, 294)
	u.win.SetTitle("Bluefin Report")

	u.nav = adw.NewNavigationView()
	u.nav.Add(u.buildOverview())
	u.buildMetrics()
	u.buildCompose()
	u.buildReview()
	u.buildDone()
	u.buildDrafts()
	u.buildSignIn()

	u.toasts = adw.NewToastOverlay()
	u.toasts.SetChild(&u.nav.Widget)
	u.win.SetContent(&u.toasts.Widget)

	action := func(name string, cb func()) *gio.SimpleAction {
		a := gio.NewSimpleAction(name, nil)
		a.ConnectActivate(hold(func(gio.SimpleAction, uintptr) { cb() }))
		u.win.AddAction(a)
		return a
	}
	u.refreshAct = action("refresh", u.refresh)
	action("about", u.about)
	action("find", func() {
		if u.nav.GetVisiblePageTag() == "metrics" {
			u.search.GrabFocus()
		}
	})
	action("close", func() { u.win.Close() })
	action("drafts", func() { u.refreshDrafts(); u.nav.PushByTag("drafts") })
	action("shortcuts", u.shortcuts)
	action("help", func() {
		log.Print("ui: opening docs")
		gtk.NewUriLauncher("https://docs.projectbluefin.io").Launch(&u.win.Window, nil, nil, 0)
	})
	action("advance", func() {
		switch u.nav.GetVisiblePageTag() {
		case "compose":
			if u.reviewBtn.GetSensitive() {
				u.openReview()
			}
		case "review":
			if u.publishBtn.GetSensitive() {
				u.publish()
			}
		}
	})
	// Draft rows are rebuilt on every refresh; their buttons target these two
	// actions with the report ID, so no per-row closure is ever connected.
	idAction := func(name string, cb func(id string)) *gio.SimpleAction {
		a := gio.NewSimpleAction(name, glib.NewVariantType("s"))
		a.ConnectActivate(hold(func(_ gio.SimpleAction, p uintptr) {
			cb((*(**glib.Variant)(unsafe.Pointer(&p))).GetString(nil))
		}))
		u.win.AddAction(a)
		return a
	}
	idAction("draft-resume", u.resume)
	u.discardAct = idAction("draft-discard", func(id string) {
		if u.publishing {
			u.toast("Wait for publishing to finish")
			return
		}
		u.discardID = id
		u.discardDlg.Present(&u.win.Widget)
	})
	quit := gio.NewSimpleAction("quit", nil)
	quit.ConnectActivate(hold(func(gio.SimpleAction, uintptr) { u.app.Quit() }))
	u.app.AddAction(quit)
	u.app.SetAccelsForAction("win.refresh", []string{"<Control>r", "F5"})
	u.app.SetAccelsForAction("win.find", []string{"<Control>f"})
	u.app.SetAccelsForAction("win.close", []string{"<Control>w"})
	u.app.SetAccelsForAction("win.shortcuts", []string{"<Control>question"})
	u.app.SetAccelsForAction("win.help", []string{"F1"})
	u.app.SetAccelsForAction("win.advance", []string{"<Control>Return"})
	u.app.SetAccelsForAction("app.quit", []string{"<Control>q"})

	u.win.Present()
	u.refreshDrafts()
	u.refresh()
}

// showError reports a failed user action in a dialog: it stays until
// dismissed and appears on whichever page the action ran from.
func (u *ui) showError(msg string) {
	log.Print("ui: error: ", msg)
	a := adw.NewAlertDialog("Something went wrong", msg)
	a.AddResponse("ok", "OK")
	a.Present(&u.win.Widget)
}

func (u *ui) toast(msg string) { u.toasts.AddToast(adw.NewToast(msg)) }

// toolbar adds the header, plus the dry-run banner below it, to a page.
func (u *ui) toolbar(header *adw.HeaderBar, content *gtk.Widget) *adw.ToolbarView {
	tv := adw.NewToolbarView()
	if header == nil {
		header = adw.NewHeaderBar()
	}
	tv.AddTopBar(&header.Widget)
	if u.dryRun {
		b := adw.NewBanner("Dry run: nothing will be published")
		b.SetRevealed(true)
		tv.AddTopBar(&b.Widget)
	}
	tv.SetContent(content)
	return tv
}

func (u *ui) page(title, tag string, header *adw.HeaderBar, content *gtk.Widget) *adw.NavigationPage {
	p := adw.NewNavigationPage(&u.toolbar(header, content).Widget, title)
	p.SetTag(tag)
	return p
}

// scrolled wraps groups in a clamped, scrollable column.
func scrolled(children ...*gtk.Widget) *gtk.Widget {
	box := gtk.NewBox(gtk.OrientationVerticalValue, 24)
	box.SetMarginTop(24)
	box.SetMarginBottom(24)
	box.SetMarginStart(12)
	box.SetMarginEnd(12)
	for _, c := range children {
		box.Append(c)
	}
	clamp := adw.NewClamp()
	clamp.SetChild(&box.Widget)
	sw := gtk.NewScrolledWindow()
	sw.SetPolicy(gtk.PolicyNeverValue, gtk.PolicyAutomaticValue)
	sw.SetChild(&clamp.Widget)
	sw.SetVexpand(true)
	return &sw.Widget
}

func group(title string) *adw.PreferencesGroup {
	g := adw.NewPreferencesGroup()
	g.SetTitle(title)
	return g
}

func pill(label string, cb func()) *gtk.Button {
	b := gtk.NewButtonWithLabel(label)
	b.AddCssClass("pill")
	b.ConnectClicked(hold(func(gtk.Button) { cb() }))
	return b
}

// ---------- Overview ----------

func (u *ui) buildOverview() *adw.NavigationPage {
	header := adw.NewHeaderBar()
	refreshBtn := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refreshBtn.SetTooltipText("Refresh (Ctrl+R)")
	refreshBtn.SetActionName("win.refresh")
	header.PackStart(&refreshBtn.Widget)
	menu := gio.NewMenu()
	menu.Append("Drafts", "win.drafts")
	menu.Append("Keyboard Shortcuts", "win.shortcuts")
	menu.Append("About Bluefin Report", "win.about")
	mb := gtk.NewMenuButton()
	mb.SetIconName("open-menu-symbolic")
	mb.SetTooltipText("Main Menu")
	mb.SetMenuModel(&menu.MenuModel)
	mb.SetPrimary(true)
	header.PackEnd(&mb.Widget)

	sys := group("This system")
	for i, t := range []string{"Name", "Version", "Image", "Kernel", "Architecture", "State"} {
		r := adw.NewActionRow()
		r.SetTitle(t)
		r.AddCssClass("property")
		u.sysRows[i] = r
		sys.Add(&r.Widget)
	}

	hg := group("Health")
	u.health = gtk.NewListBox()
	u.health.SetSelectionMode(gtk.SelectionNoneValue)
	u.health.AddCssClass("boxed-list")
	hg.Add(&u.health.Widget)

	help := group("Get help")
	u.helpGroup = help
	btns := gtk.NewFlowBox() // two pills side by side, stacked when narrow
	btns.SetSelectionMode(gtk.SelectionNoneValue)
	btns.SetMaxChildrenPerLine(2)
	btns.SetColumnSpacing(12)
	btns.SetRowSpacing(12)
	btns.SetHalign(gtk.AlignCenterValue)
	btns.Append(&pill("Report a Problem", func() { u.openCompose(publish.NewIssue) }).Widget)
	btns.Append(&pill("Confirm Existing Issue", func() { u.openCompose(publish.Confirm) }).Widget)
	help.Add(&btns.Widget)
	btns.SetSensitive(false) // until a snapshot tells us which repo this image reports to
	u.helpBtns = btns

	browse := adw.NewActionRow()
	browse.SetTitle("Browse all metrics")
	browse.SetActivatable(true)
	browse.AddSuffix(&gtk.NewImageFromIconName("go-next-symbolic").Widget)
	browse.ConnectActivated(hold(func(adw.ActionRow) { u.nav.PushByTag("metrics") }))
	bg := adw.NewPreferencesGroup()
	u.draftsRow = adw.NewActionRow()
	u.draftsRow.SetActivatable(true)
	u.draftsRow.AddPrefix(&gtk.NewImageFromIconName("document-edit-symbolic").Widget)
	u.draftsRow.AddSuffix(&gtk.NewImageFromIconName("go-next-symbolic").Widget)
	u.draftsRow.SetActionName("win.drafts")
	u.draftsRow.SetVisible(false)
	bg.Add(&u.draftsRow.Widget)
	bg.Add(&browse.Widget)

	u.spinner = adw.NewSpinner()
	loading := adw.NewStatusPage()
	loading.SetTitle("Collecting system report…")
	loading.SetChild(&u.spinner.Widget)

	u.statusPage = adw.NewStatusPage()
	u.statusPage.SetIconName("dialog-warning-symbolic")

	u.ovStack = gtk.NewStack()
	u.ovStack.AddNamed(&loading.Widget, "loading")
	u.ovStack.AddNamed(scrolled(&sys.Widget, &hg.Widget, &help.Widget, &bg.Widget), "ready")
	u.ovStack.AddNamed(&u.statusPage.Widget, "error")
	return u.page("Bluefin Report", "overview", header, &u.ovStack.Widget)
}

func (u *ui) refresh() {
	if u.cancel != nil {
		u.cancel()
	}
	u.gen++
	gen := u.gen
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	u.cancel = cancel
	u.snap = nil
	u.reviewBtn.SetSensitive(false)
	u.ovStack.SetVisibleChildName("loading")
	u.refreshAct.SetEnabled(false)
	log.Printf("ui: collecting snapshot (gen %d)", gen)
	go func() {
		defer cancel()
		start := time.Now()
		s, err := snapshot.Take(ctx, snapshot.Run, nil)
		post(func() {
			if gen != u.gen {
				log.Printf("ui: dropping stale snapshot gen %d", gen)
				return
			}
			u.refreshAct.SetEnabled(true)
			log.Printf("ui: snapshot gen %d in %s: %d system, %d user metrics, err=%v",
				gen, time.Since(start).Round(time.Millisecond), len(s.System.Metrics), len(s.User.Metrics), err)
			u.loaded(s, err)
		})
	}()
}

func (u *ui) loaded(s snapshot.Snapshot, err error) {
	if err != nil {
		u.snap = nil
		if errors.Is(err, snapshot.ErrNotInstalled) {
			u.statusPage.SetTitle("System reports are unavailable")
			u.statusPage.SetDescription(snapshot.Binary + " was not found. This system needs systemd 262 or newer.")
		} else {
			u.statusPage.SetTitle("Couldn't collect a report")
			u.statusPage.SetDescription(err.Error())
		}
		u.ovStack.SetVisibleChildName("error")
		return
	}
	u.snap = &s
	u.sum = s.Summary()
	u.repoName = publish.Route(u.sum.ImageID)
	u.helpBtns.SetSensitive(u.repoName != "")
	if u.repoName == "" {
		u.helpGroup.SetDescription("Reporting isn't available for this image (" + u.sum.ImageID + "). Only Dakota and Utah accept reports.")
	} else {
		u.helpGroup.SetDescription("Reports go to " + u.repoName + ", the repository for the image this system runs.")
	}
	for i, v := range []string{u.sum.Name, u.sum.ImageVersion, u.sum.ImageID, u.sum.Kernel, u.sum.Arch, u.sum.SystemState} {
		if v == "" {
			v = "Unknown"
		}
		u.sysRows[i].SetSubtitle(v)
	}

	u.health.RemoveAll()
	add := func(title, sub, icon string) {
		r := adw.NewActionRow()
		r.SetTitle(title)
		r.SetSubtitle(sub)
		r.SetTitleSelectable(true)
		r.AddPrefix(&gtk.NewImageFromIconName(icon).Widget)
		u.health.Append(&r.Widget)
	}
	for _, f := range u.sum.FailedUnits {
		add(f, "Failed unit", "dialog-error-symbolic")
	}
	for _, w := range u.sum.Unreachable {
		add(w, "Source unreachable", "network-offline-symbolic")
	}
	for _, w := range u.sum.Warnings {
		if !slices.Contains(u.sum.Unreachable, w) { // Unreachable is a subset of Warnings
			add(w, "Warning", "dialog-warning-symbolic")
		}
	}
	if len(u.sum.FailedUnits)+len(u.sum.Warnings) == 0 {
		add("No problems found", "No failed units or warnings", "emblem-ok-symbolic")
	}

	rows := make([]string, 0, len(s.System.Metrics)+len(s.User.Metrics))
	row := func(prefix string, m snapshot.Metric) string {
		name := prefix + m.Name
		if m.Object != "" {
			name += " [" + m.Object + "]"
		}
		return name + "\n" + m.String() // bind splits name / value onto two lines
	}
	for _, m := range s.System.Metrics {
		rows = append(rows, row("", m))
	}
	for _, m := range s.User.Metrics {
		rows = append(rows, row("user: ", m))
	}
	u.metrics.Splice(0, u.metrics.GetNItems(), rows)
	u.ovStack.SetVisibleChildName("ready")
	u.syncReview()
}

// ---------- Metrics ----------

func (u *ui) buildMetrics() {
	u.metrics = gtk.NewStringList(nil)
	expr := gtk.NewPropertyExpression(gtk.StringObjectGLibType(), nil, "string")
	u.filter = gtk.NewStringFilter(&expr.Expression)
	u.filter.SetMatchMode(gtk.StringFilterMatchModeSubstringValue)
	u.filter.SetIgnoreCase(true)
	model := gtk.NewFilterListModel(u.metrics, &u.filter.Filter)

	f := gtk.NewSignalListItemFactory()
	f.ConnectSetup(hold(func(_ gtk.SignalListItemFactory, p uintptr) {
		box := gtk.NewBox(gtk.OrientationVerticalValue, 2)
		box.SetMarginStart(12)
		box.SetMarginEnd(12)
		for _, mode := range []pango.EllipsizeMode{pango.EllipsizeMiddleValue, pango.EllipsizeEndValue} { // name, value
			l := gtk.NewLabel("")
			l.SetXalign(0)
			l.SetEllipsize(mode)
			l.AddCssClass("monospace")
			if mode == pango.EllipsizeEndValue {
				l.AddCssClass("dim-label")
				l.AddCssClass("caption")
			}
			box.Append(&l.Widget)
		}
		gtk.ListItemNewFromInternalPtr(p).SetChild(&box.Widget)
	}))
	f.ConnectBind(hold(func(_ gtk.SignalListItemFactory, p uintptr) {
		li := gtk.ListItemNewFromInternalPtr(p)
		s := gtk.StringObjectNewFromInternalPtr(li.GetItem().GoPointer()).GetString()
		name, val, _ := strings.Cut(s, "\n")
		box := li.GetChild()
		gtk.LabelNewFromInternalPtr(box.GetFirstChild().GoPointer()).SetText(name)
		gtk.LabelNewFromInternalPtr(box.GetLastChild().GoPointer()).SetText(val)
		box.SetTooltipText(name + " = " + val)
	}))
	lv := gtk.NewListView(gtk.NewNoSelection(model), &f.ListItemFactory)
	lv.AddCssClass("navigation-sidebar")
	sw := gtk.NewScrolledWindow()
	sw.SetChild(&lv.Widget)
	sw.SetVexpand(true)

	search := gtk.NewSearchEntry()
	u.search = search
	search.SetPlaceholderText("Filter metrics")
	search.ConnectSearchChanged(hold(func(gtk.SearchEntry) { u.filter.SetSearch(search.GetText()) }))
	search.SetMarginTop(6)
	search.SetMarginBottom(6)
	search.SetMarginStart(12)
	search.SetMarginEnd(12)

	box := gtk.NewBox(gtk.OrientationVerticalValue, 0)
	box.Append(&search.Widget)
	box.Append(&sw.Widget)
	u.nav.Add(u.page("Metrics", "metrics", nil, &box.Widget))
}

// ---------- Compose ----------

func (u *ui) buildCompose() {
	g := adw.NewPreferencesGroup()
	u.repo = adw.NewActionRow()
	u.repo.SetTitle("Project")
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

	dg := group("What happened?")
	u.desc = gtk.NewTextBuffer(nil)
	tv := gtk.NewTextViewWithBuffer(u.desc)
	tv.SetWrapMode(gtk.WrapWordCharValue)
	tv.SetTopMargin(12)
	tv.SetBottomMargin(12)
	tv.SetLeftMargin(12)
	tv.SetRightMargin(12)
	tv.SetSizeRequest(-1, 160)
	tv.AddCssClass("card")
	dg.Add(&tv.Widget)

	og := group("Options")
	u.attach = adw.NewSwitchRow()
	u.attach.SetTitle("Attach system report")
	u.attach.SetSubtitle("Reviewed before anything is sent")
	u.attach.SetActive(true)
	og.Add(&u.attach.Widget)
	u.includeUser = adw.NewSwitchRow()
	u.includeUser.SetTitle("Include user session")
	og.Add(&u.includeUser.Widget)
	u.queue = adw.NewComboRow()
	u.queue.SetTitle("Who should look at it")
	u.queue.SetModel(gtk.NewStringList([]string{"No preference", "Machine analysis", "Human only"}))
	og.Add(&u.queue.Widget)

	u.reviewBtn = pill("Review", u.openReview)
	u.reviewBtn.AddCssClass("suggested-action")
	u.reviewBtn.SetHalign(gtk.AlignCenterValue)
	u.reviewBtn.SetSensitive(false)

	u.newOnly = []*gtk.Widget{&u.title.Widget, &dg.Widget, &u.queue.Widget, &u.attach.Widget}
	u.composePage = u.page("Report a Problem", "compose", nil, scrolled(&g.Widget, &dg.Widget, &og.Widget, &u.reviewBtn.Widget))
	u.nav.Add(u.composePage)
}

// syncReview enables Review once the snapshot is ready and the mode's
// required field is filled.
func (u *ui) syncReview() {
	if u.reviewBtn == nil {
		return // notify fires while the page is still being built
	}
	ok := strings.TrimSpace(u.title.GetText()) != ""
	msg := ""
	if u.mode == publish.Confirm {
		ref := strings.TrimSpace(u.issueRef.GetText())
		_, err := publish.ParseIssueRef(u.repoName, ref)
		ok = err == nil
		if err != nil && ref != "" {
			msg = err.Error()
		}
	}
	u.refErr.SetText(msg)
	u.refErr.SetVisible(msg != "")
	u.reviewBtn.SetSensitive(u.snap != nil && ok)
}

func (u *ui) openCompose(m publish.Mode) {
	u.mode = m
	newIssue := m == publish.NewIssue
	for _, w := range u.newOnly {
		w.SetVisible(newIssue)
	}
	u.issueRef.SetVisible(!newIssue)
	u.attach.SetActive(newIssue) // Confirm only comments; it never attaches
	u.title.SetText("")
	u.issueRef.SetText("")
	u.desc.SetText("", -1)
	if newIssue {
		u.composePage.SetTitle("Report a Problem")
	} else {
		u.composePage.SetTitle("Confirm Existing Issue")
	}
	u.repo.SetSubtitle(u.repoName)
	u.syncReview()
	u.nav.PushByTag("compose")
}

// ---------- Review ----------

func monoView(buf *gtk.TextBuffer, editable bool) *gtk.Widget {
	tv := gtk.NewTextViewWithBuffer(buf)
	tv.SetMonospace(true)
	tv.SetEditable(editable)
	tv.SetWrapMode(gtk.WrapWordCharValue)
	tv.SetTopMargin(12)
	tv.SetBottomMargin(12)
	tv.SetLeftMargin(12)
	tv.SetRightMargin(12)
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
			u.consentLink.SetActive(false)
		}
	}))
	u.attachText = gtk.NewTextBuffer(nil)
	u.removed = gtk.NewListBox()
	u.removed.SetSelectionMode(gtk.SelectionNoneValue)
	u.removed.AddCssClass("boxed-list")

	stack := adw.NewViewStack()
	stack.AddTitledWithIcon(monoView(u.issueText, true), "issue", "Issue text", "document-edit-symbolic")
	stack.AddTitledWithIcon(monoView(u.attachText, false), "attachment", "Attachment", "mail-attachment-symbolic")
	rg := group("What was removed")
	rg.SetDescription("Values scrubbed or dropped before sharing")
	rg.Add(&u.removed.Widget)
	stack.AddTitledWithIcon(scrolled(&rg.Widget), "removed", "Removed", "edit-clear-symbolic")
	stack.SetSizeRequest(-1, 280) // outer scroll keeps Consent reachable at 360×294

	header := adw.NewHeaderBar()
	sw := adw.NewViewSwitcher()
	sw.SetStack(stack)
	sw.SetPolicy(adw.ViewSwitcherPolicyNarrowValue) // fits 360 px
	header.SetTitleWidget(&sw.Widget)

	cg := group("Consent")
	u.consentIssue = adw.NewSwitchRow()
	u.consentIssue.SetTitle("I understand the issue text will be public")
	u.consentLink = adw.NewSwitchRow()
	u.consentLink.SetTitle("I understand anyone with the attachment link can read it")
	sync := hold(func(gobject.Object, uintptr) { u.syncPublish() })
	u.consentIssue.ConnectNotify(sync)
	u.consentLink.ConnectNotify(sync)
	cg.Add(&u.consentIssue.Widget)
	cg.Add(&u.consentLink.Widget)

	u.saveDialog = gtk.NewFileDialog()
	u.saveDialog.SetTitle("Export Report")
	saved := hold(gio.AsyncReadyCallback(func(_, res, _ uintptr) {
		f, err := u.saveDialog.SaveFinish(gio.SimpleAsyncResultNewFromInternalPtr(res))
		if err != nil || f == nil {
			return // cancelled
		}
		path := f.GetPath()
		if err := os.WriteFile(path, u.payload.JSON, 0o600); err != nil {
			u.showError("Export failed: " + err.Error())
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
	btns := gtk.NewBox(gtk.OrientationHorizontalValue, 12)
	btns.SetHalign(gtk.AlignCenterValue)
	btns.Append(&export.Widget)
	btns.Append(&u.publishBtn.Widget)

	btns.SetMarginTop(6)
	btns.SetMarginBottom(6)

	tv := u.toolbar(header, scrolled(&stack.Widget, &cg.Widget))
	tv.AddBottomBar(&btns.Widget)
	p := adw.NewNavigationPage(&tv.Widget, "Review")
	p.SetTag("review")
	u.nav.Add(p)
}

func (u *ui) syncPublish() {
	if u.publishBtn == nil {
		return // notify fires while the page is still being built
	}
	ok := !u.publishing && u.consentIssue.GetActive() && (!u.attach.GetActive() || u.consentLink.GetActive())
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

func (u *ui) openReview() {
	name := ""
	if cur, err := user.Current(); err == nil {
		name = cur.Username
	}
	// Synchronous on purpose: measured ~40 ms (9.5k metrics, 725 KB payload) on a dev host.
	p, err := privacy.Build(*u.snap, privacy.Options{Username: name, IncludeUser: u.includeUser.GetActive()})
	if err != nil {
		u.showError("Couldn't prepare the report: " + err.Error())
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

	u.consentIssue.SetActive(false)
	u.consentLink.SetActive(false)
	u.consentLink.SetVisible(u.attach.GetActive())
	u.syncPublish()
	u.nav.PushByTag("review")
}

func (u *ui) markdown() string {
	var b strings.Builder
	if u.mode == publish.NewIssue {
		if d := strings.TrimSpace(bufText(u.desc)); d != "" {
			b.WriteString(d + "\n\n")
		}
	}
	b.WriteString("| | |\n|---|---|\n")
	for _, kv := range [][2]string{
		{"System", u.sum.Name}, {"Version", u.sum.ImageVersion}, {"Image", u.sum.ImageID},
		{"Kernel", u.sum.Kernel}, {"Arch", u.sum.Arch}, {"State", u.sum.SystemState},
		{"Failed units", fmt.Sprint(len(u.sum.FailedUnits))},
	} {
		fmt.Fprintf(&b, "| %s | %s |\n", kv[0], kv[1])
	}
	for _, f := range u.sum.FailedUnits {
		fmt.Fprintf(&b, "\n- `%s`", f)
	}
	if u.attach.GetActive() {
		b.WriteString("\n\nFull report attached.\n")
	}
	return b.String()
}

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
	} else {
		d.IssueRef = strings.TrimSpace(u.issueRef.GetText())
	}
	if u.attach.GetActive() {
		d.Attachment = u.payload.JSON
	}
	log.Printf("ui: publishing %s to %s (mode %d, %d attachment bytes)", d.ReportID, d.Repo, d.Mode, len(d.Attachment))
	u.run(func() { u.publish() }, func(ctx context.Context, gh publish.Exec) (publish.Result, error) {
		return publish.Submit(ctx, gh, u.stateDir, d)
	})
}

func (u *ui) resume(id string) {
	if u.publishing {
		return
	}
	log.Printf("ui: resuming %s", id)
	u.run(func() { u.resume(id) }, func(ctx context.Context, gh publish.Exec) (publish.Result, error) {
		return publish.Resume(ctx, gh, u.stateDir, id)
	})
}

// run does one publish step off the main thread; retry repeats it after sign-in.
func (u *ui) run(retry func(), step func(context.Context, publish.Exec) (publish.Result, error)) {
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
			case err != nil:
				u.refreshDrafts()
				u.showError("Publishing failed: " + err.Error() + "\n\nYour draft was kept; resume it from Drafts.")
			default:
				u.refreshDrafts()
				u.doneURL = res.IssueURL
				u.donePage.SetDescription(res.IssueURL)
				u.title.SetText("")
				u.issueRef.SetText("")
				u.desc.SetText("", -1)
				u.nav.ReplaceWithTags([]string{"overview", "done"}, 2) // Back from Done goes home, not to Publish
			}
		})
	}()
}

// ---------- Drafts ----------

func (u *ui) buildDrafts() {
	u.draftList = gtk.NewListBox()
	u.draftList.SetSelectionMode(gtk.SelectionNoneValue)
	u.draftList.AddCssClass("boxed-list")
	g := group("Unfinished reports")
	g.SetDescription("Discarding only deletes the local copy, never anything already on GitHub.")
	g.Add(&u.draftList.Widget)
	u.nav.Add(u.page("Drafts", "drafts", nil, scrolled(&g.Widget)))

	u.discardDlg = adw.NewAlertDialog("Discard draft?", "The local copy is deleted. Anything already published stays on GitHub.")
	u.discardDlg.Ref() // reused: keep it alive between presentations
	u.discardDlg.AddResponse("cancel", "Cancel")
	u.discardDlg.AddResponse("discard", "Discard")
	u.discardDlg.SetResponseAppearance("discard", adw.ResponseDestructiveValue)
	u.discardDlg.SetCloseResponse("cancel")
	u.discardDlg.ConnectResponse(hold(func(_ adw.AlertDialog, r string) {
		if r != "discard" || u.publishing {
			return
		}
		log.Printf("ui: discarding draft %s", u.discardID)
		if err := publish.Discard(u.stateDir, u.discardID); err != nil {
			u.showError("Couldn't discard: " + err.Error())
		}
		u.refreshDrafts()
		u.toast("Draft discarded")
	}))
}

// refreshDrafts rebuilds the Drafts list and the Overview row from disk.
func (u *ui) refreshDrafts() {
	ds, err := publish.Pending(u.stateDir)
	if err != nil {
		log.Print("ui: drafts: ", err)
	}
	log.Printf("ui: %d unfinished drafts", len(ds))
	u.draftsRow.SetTitle(fmt.Sprintf("Unfinished reports (%d)", len(ds)))
	u.draftsRow.SetVisible(len(ds) > 0)
	u.draftList.RemoveAll()
	for _, s := range ds {
		r := adw.NewActionRow()
		t := s.Draft.Title
		if t == "" {
			t = "Comment on " + s.Draft.IssueRef
		}
		r.SetTitle(t)
		r.SetSubtitle(s.Draft.Repo + " · " + string(s.Stage))
		btn := func(icon, tip, act string) {
			b := gtk.NewButtonFromIconName(icon)
			b.SetTooltipText(tip)
			b.SetValign(gtk.AlignCenterValue)
			b.AddCssClass("flat")
			b.SetActionName(act)
			b.SetActionTargetValue(glib.NewVariantString(s.ReportID))
			r.AddSuffix(&b.Widget)
		}
		btn("media-playback-start-symbolic", "Resume", "win.draft-resume")
		btn("user-trash-symbolic", "Discard", "win.draft-discard")
		u.draftList.Append(&r.Widget)
	}
	if len(ds) == 0 {
		r := adw.NewActionRow()
		r.SetTitle("No unfinished reports")
		u.draftList.Append(&r.Widget)
	}
}

// ---------- Sign-in ----------

func (u *ui) buildSignIn() {
	u.signInDlg = adw.NewAlertDialog("Sign in to GitHub",
		"Publishing needs a GitHub account. Your browser opens to approve Bluefin Report; your draft is kept.")
	u.signInDlg.Ref()
	u.signInDlg.AddResponse("cancel", "Cancel")
	u.signInDlg.AddResponse("signin", "Sign In")
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
	cp := pill("Copy Code", func() {
		u.win.GetClipboard().SetText(u.codeLabel.GetText())
		u.toast("Code copied")
	})
	open := pill("Open Browser", func() {
		gtk.NewUriLauncher(u.codeURL).Launch(&u.win.Window, nil, nil, 0)
	})
	open.AddCssClass("suggested-action")
	btns := gtk.NewFlowBox()
	btns.SetSelectionMode(gtk.SelectionNoneValue)
	btns.SetMaxChildrenPerLine(2)
	btns.SetColumnSpacing(12)
	btns.SetRowSpacing(12)
	btns.SetHalign(gtk.AlignCenterValue)
	btns.Append(&cp.Widget)
	btns.Append(&open.Widget)
	box := gtk.NewBox(gtk.OrientationVerticalValue, 12)
	box.Append(&u.codeLabel.Widget)
	box.Append(&btns.Widget)

	u.codeDlg = adw.NewAlertDialog("Enter this code on GitHub", "Copy the code, open the browser, and paste it there. This closes by itself once GitHub confirms.")
	u.codeDlg.Ref()
	u.codeDlg.SetExtraChild(&box.Widget)
	u.codeDlg.AddResponse("cancel", "Cancel")
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
					u.showError("Sign-in failed: " + err.Error())
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

// ---------- Shortcuts ----------

func (u *ui) shortcuts() {
	s := adw.NewShortcutsSection("")
	for _, kv := range [][2]string{
		{"Refresh snapshot", "<Control>r F5"},
		{"Search metrics", "<Control>f"},
		{"Back", "<Alt>Left Escape"},
		{"Advance (Compose → Review → Publish)", "<Control>Return"},
		{"Keyboard shortcuts", "<Control>question"},
		{"Help", "F1"},
		{"Close window", "<Control>w"},
		{"Quit", "<Control>q"},
	} {
		s.Add(adw.NewShortcutsItem(kv[0], kv[1]))
	}
	d := adw.NewShortcutsDialog()
	d.Add(s)
	d.Present(&u.win.Widget)
}

func stateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "bluefin-report", "drafts")
}

// fakeGH records gh calls on stderr and returns plausible URLs (--dry-run).
func fakeGH(_ context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	log.Printf("dry-run gh %q (stdin %d bytes)", args, len(stdin))
	switch {
	case len(args) > 1 && args[0] == "gist" && args[1] == "create":
		return []byte("https://gist.github.com/dry-run/0000\n"), nil, nil
	case len(args) > 1 && args[0] == "issue":
		return []byte("https://github.com/projectbluefin/dry-run/issues/1\n"), nil, nil
	}
	return nil, nil, nil
}

// fakeGHStream signs in instantly (--dry-run).
func fakeGHStream(_ context.Context, _ []byte, onLine func(string), args ...string) error {
	log.Printf("dry-run gh %q", args)
	onLine("! First copy your one-time code: DRY0-RUN0")
	onLine("Open this URL to continue in your web browser: https://github.com/login/device")
	return nil
}

// ---------- Done ----------

func (u *ui) buildDone() {
	u.donePage = adw.NewStatusPage()
	u.donePage.SetIconName("emblem-ok-symbolic")
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
	btns.SetHalign(gtk.AlignCenterValue)
	btns.Append(&open.Widget)
	btns.Append(&cp.Widget)
	u.donePage.SetChild(&btns.Widget)
	u.nav.Add(u.page("Done", "done", nil, &u.donePage.Widget))
}

func (u *ui) about() {
	a := adw.NewAboutDialog()
	a.SetApplicationName("Bluefin Report")
	a.SetApplicationIcon(appID)
	a.SetDeveloperName("Project Bluefin")
	a.SetVersion("0.1")
	a.SetWebsite("https://github.com/projectbluefin/bluefin-report")
	a.SetLicenseType(gtk.LicenseApache20Value)
	a.Present(&u.win.Widget)
}
