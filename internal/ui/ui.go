// Package ui is the GTK4/Libadwaita front end of Bluefin Report.
//
// puregotk callbacks live in a fixed, never-freed table, so every signal is
// connected exactly once in build*; refreshes only mutate widgets and models.
package ui

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
	"unsafe"

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

const appID = "io.projectbluefin.Report"

// appName is the user-visible name; data/*.desktop and metainfo repeat it.
const appName = "Report a Problem"

// appIcon is the icon name used by the About dialog; data/*.desktop Icon= must match (checked by a test).
const appIcon = "io.projectbluefin.Report"

// previewBytes caps the Attachment tab; exported/published bytes are always full.
const previewBytes = 200 << 10

var queues = []string{"", "3-clanker-queue", "3-human-queue"}

var areas = []string{"", "desktop", "flatpak", "gaming", "hardware", "installer", "dx"}

// Version is set at release build time via -ldflags -X.
var Version = "dev"

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
	sysRows    [12]*adw.ActionRow
	ident      extras.Identity
	health     *gtk.ListBox

	// metrics
	metrics     *gtk.StringList
	filter      *gtk.StringFilter
	search      *gtk.SearchEntry
	syncResults func() // filtered n-items notify misses 0→0 when the store empties under a query

	// compose
	mode        publish.Mode
	composePage *adw.NavigationPage
	repo        *adw.ActionRow // shows repoName; there is no choice: reports go to the running image's repo
	repoName    string
	reportDesc  *gtk.Label
	reportActs  []*gtk.Widget         // disabled until the snapshot names this image's repo
	helpGroup   *adw.PreferencesGroup // troubleshooting help; shown only when Overview reports a problem
	sysDetails  *adw.ExpanderRow
	title       *adw.EntryRow
	issueRef    *adw.EntryRow
	desc        *gtk.TextBuffer
	steps       *gtk.TextBuffer
	optIn       []*adw.SwitchRow // parallel to extras.Sections
	attach      *adw.SwitchRow
	includeUser *adw.SwitchRow
	queue       *adw.ComboRow
	area        *adw.ComboRow
	newOnly     []*gtk.Widget
	descGroup   *adw.PreferencesGroup
	reviewBtn   *gtk.Button
	refErr      *gtk.Label
	shots       []string // screenshot paths chosen on Compose
	descView    *gtk.TextView
	addShot     *adw.ButtonRow // last row of shotList
	shotList    *gtk.ListBox
	shotDialog  *gtk.FileDialog

	// review
	payload       privacy.Payload
	reportID      string
	draftID       string // the Unfinished draft this Compose form was saved to or restored from
	detachedDraft string // draftID before its discard, restored if the discard is undone
	publishing    bool
	issueText     *gtk.TextBuffer
	settingText   bool
	attachText    *gtk.TextBuffer
	removed       *gtk.ListBox
	consentIssue  *adw.SwitchRow
	consentReport *adw.SwitchRow
	publishBtn    *gtk.Button
	saveDialog    *gtk.FileDialog
	shotsGroup    *adw.PreferencesGroup
	consentGroup  *adw.PreferencesGroup

	// done
	donePage *adw.StatusPage
	doneURL  string

	// drafts
	draftsRow   *adw.ActionRow
	draftsCount *gtk.Label
	draftsGrp   *adw.PreferencesGroup
	draftList   *gtk.ListBox
	draftsStack *gtk.Stack // "list" or "empty"
	discardAct  *gio.SimpleAction
	discarded   map[uintptr]string // undo toast → report ID: hidden from Drafts, deleted when the toast goes without Undo
	undoToast   *adw.Toast         // the showing Undo toast; any new toast dismisses it, since normal toasts queue behind it
	onUndo      *func(adw.Toast)   // connected to every undo toast; one callback for all, keyed by toast pointer
	onDismissed *func(adw.Toast)

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
	u.win.SetSizeRequest(360, 294)
	ws := windowState{Width: 720, Height: 760}
	if !u.dryRun { // dry runs (tests, screenshots) always use the default size
		ws = loadWindowState()
	}
	u.win.SetDefaultSize(int32(ws.Width), int32(ws.Height))
	if ws.Maximized {
		u.win.Maximize()
	}
	u.win.ConnectCloseRequest(hold(func(gtk.Window) bool {
		u.toasts.DismissAll() // runs pending discards: closing is not an Undo
		u.saveCompose(false)
		if !u.dryRun {
			saveWindowState(&u.win.Window)
		}
		return false
	}))
	u.win.SetTitle(appName)

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
	action("save-draft", func() { u.saveCompose(true) })
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
	// Rows rebuilt on refresh target these string-parameter actions (report ID,
	// metric value, screenshot path), so no per-row closure is ever connected.
	stringAction := func(name string, cb func(s string)) *gio.SimpleAction {
		a := gio.NewSimpleAction(name, glib.NewVariantType("s"))
		a.ConnectActivate(hold(func(_ gio.SimpleAction, p uintptr) {
			cb((*(**glib.Variant)(unsafe.Pointer(&p))).GetString(nil))
		}))
		u.win.AddAction(a)
		return a
	}
	stringAction("copy-metric", func(value string) {
		u.win.GetClipboard().SetText(value)
		u.toast("Metric value copied")
	})
	stringAction("draft-resume", u.resume)
	u.discardAct = stringAction("draft-discard", func(id string) {
		if u.publishing {
			u.toast("Wait for publishing to finish")
			return
		}
		t := adw.NewToast("Report discarded")
		t.SetButtonLabel("_Undo")
		t.SetTimeout(0) // stays until Undo, close or the next toast: 5 s is too short to reach by keyboard
		t.ConnectButtonClicked(u.onUndo)
		t.ConnectDismissed(u.onDismissed)
		u.discarded[t.Ptr] = id
		u.detachCompose(id)
		log.Printf("ui: draft %s discarded, undo available", id)
		u.refreshDrafts()
		u.addToast(t)
		u.undoToast = t
	})
	stringAction("shot-remove", func(path string) {
		u.shots = slices.DeleteFunc(u.shots, func(p string) bool { return p == path })
		u.syncShots()
	})
	quit := gio.NewSimpleAction("quit", nil)
	// Close, not Quit: close-request saves the window size and runs pending discards.
	quit.ConnectActivate(hold(func(gio.SimpleAction, uintptr) { u.win.Close() }))
	u.app.AddAction(quit)
	u.app.SetAccelsForAction("win.refresh", []string{"<Control>r", "F5"})
	u.app.SetAccelsForAction("win.find", []string{"<Control>f"})
	u.app.SetAccelsForAction("win.close", []string{"<Control>w"})
	u.app.SetAccelsForAction("win.shortcuts", []string{"<Control>question"})
	u.app.SetAccelsForAction("win.help", []string{"F1"})
	u.app.SetAccelsForAction("win.save-draft", []string{"<Control>s"})
	u.app.SetAccelsForAction("win.advance", []string{"<Control>Return"})
	u.app.SetAccelsForAction("app.quit", []string{"<Control>q"})

	u.win.Present()
	u.refreshDrafts()
	u.refresh()
}

// showError reports a failed user action in a dialog: it stays until
// dismissed and appears on whichever page the action ran from.
func (u *ui) showError(heading, body string) {
	log.Printf("ui: error: %s: %s", heading, body)
	if r, n := utf8.DecodeRuneInString(body); n > 0 { // Go errors start lowercase; dialog bodies are sentences
		body = string(unicode.ToUpper(r)) + body[n:]
	}
	a := adw.NewAlertDialog(heading, body)
	a.AddResponse("close", "_Close")
	a.SetDefaultResponse("close")
	a.Present(&u.win.Widget)
}

func (u *ui) toast(msg string) { u.addToast(adw.NewToast(msg)) }

// addToast shows t at once: a pending Undo toast never times out, so it is dismissed
// (committing its discard) instead of holding every later toast in the queue.
func (u *ui) addToast(t *adw.Toast) {
	if u.undoToast != nil {
		u.undoToast.Dismiss()
	}
	u.toasts.AddToast(t)
}

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

// ---------- Shortcuts ----------

func (u *ui) shortcuts() {
	s := adw.NewShortcutsSection("")
	for _, kv := range [][2]string{
		{"Refresh snapshot", "<Control>r F5"},
		{"Search metrics", "<Control>f"},
		{"Back", "<Alt>Left Escape"},
		{"Continue to Review or Publish", "<Control>Return"},
		{"Save report as a draft", "<Control>s"},
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

// windowState is the window size kept across runs, as GNOME apps do.
type windowState struct {
	Width, Height int
	Maximized     bool
}

func windowFile() string { return filepath.Join(filepath.Dir(stateDir()), "window.json") }

// loadWindowState returns the saved size, or the default when there is none or
// it is unreadable; it never goes below the window's minimum size.
func loadWindowState() windowState {
	ws := windowState{Width: 720, Height: 760}
	if b, err := os.ReadFile(windowFile()); err == nil && json.Unmarshal(b, &ws) != nil {
		ws = windowState{Width: 720, Height: 760}
	}
	ws.Width, ws.Height = max(ws.Width, 360), max(ws.Height, 294)
	return ws
}

// saveWindowState records w's unmaximized size and whether it is maximized.
func saveWindowState(w *gtk.Window) {
	var width, height int32
	w.GetDefaultSize(&width, &height)
	b, _ := json.Marshal(windowState{Width: int(width), Height: int(height), Maximized: w.IsMaximized()})
	p := windowFile()
	err := os.MkdirAll(filepath.Dir(p), 0o700)
	if err == nil {
		err = os.WriteFile(p, b, 0o600)
	}
	if err != nil {
		log.Print("ui: saving window size: ", err)
		return
	}
	log.Printf("ui: saved window size %s", b)
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
	case len(args) > 1 && args[0] == "api" && args[1] == "user":
		return []byte("dry-run-user\n"), nil, nil
	case len(args) > 1 && args[0] == "api" && strings.HasPrefix(args[1], "repos/"):
		return []byte("dry-run-user\n"), nil, nil // Update: the signed-in user opened the issue
	case len(args) > 1 && args[0] == "api" && args[1] == "--method":
		return []byte("https://github.com/projectbluefin/dry-run/issues/1 kind/bug\n"), nil, nil
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

func (u *ui) about() {
	a := adw.NewAboutDialog()
	a.SetApplicationName(appName)
	a.SetApplicationIcon(appIcon)
	a.SetDeveloperName("Project Bluefin")
	a.SetVersion(Version)
	a.SetWebsite("https://github.com/projectbluefin/bluefin-report")
	a.SetIssueUrl("https://github.com/projectbluefin/bluefin-report/issues")
	a.SetLicenseType(gtk.LicenseApache20Value)
	a.Present(&u.win.Widget)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// a11yLabel names w for assistive technologies; a placeholder or a nearby heading is not a name.
func a11yLabel(w *gtk.Widget, s string) {
	var v gobject.Value
	v.Init(gobject.TypeStringVal)
	v.SetString(s)
	w.UpdatePropertyValue(1, []gtk.AccessibleProperty{gtk.AccessiblePropertyLabelValue}, []gobject.Value{v})
}
