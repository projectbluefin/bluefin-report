package ui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"slices"
	"strings"
	"time"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"
	"codeberg.org/puregotk/puregotk/v4/pango"

	"github.com/projectbluefin/bluefin-report/internal/extras"
	"github.com/projectbluefin/bluefin-report/internal/i18n"
	"github.com/projectbluefin/bluefin-report/internal/publish"
	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

// ---------- Overview ----------

func (u *ui) buildOverview() *adw.NavigationPage {
	header := adw.NewHeaderBar()
	refreshBtn := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refreshBtn.SetTooltipText(i18n.G("Refresh"))
	refreshBtn.SetActionName("win.refresh")
	header.PackStart(&refreshBtn.Widget)
	menu := gio.NewMenu()
	section := func(items ...[2]string) {
		s := gio.NewMenu()
		for _, it := range items {
			s.Append(it[0], it[1])
		}
		item := gio.NewMenuItemSection("", &s.MenuModel)
		item.SetAttributeValue("label", nil) // unlabeled: a separator, not a blank header
		menu.AppendItem(item)
	}
	section([2]string{i18n.G("_Drafts"), "win.drafts"})
	section([2]string{i18n.G("_Keyboard Shortcuts"), "win.shortcuts"}, [2]string{i18n.G("_Help"), "win.help"},
		// TRANSLATORS: %s is the app name, "Report a Problem".
		[2]string{fmt.Sprintf(i18n.G("_About %s"), i18n.G(appName)), "win.about"})
	mb := gtk.NewMenuButton()
	mb.SetIconName("open-menu-symbolic")
	mb.SetTooltipText(i18n.G("Main Menu"))
	mb.SetMenuModel(&menu.MenuModel)
	mb.SetPrimary(true)
	header.PackEnd(&mb.Widget)

	// Primary task: one call to action in open space (HIG: pill + suggested for the view's main action).
	heading := gtk.NewLabel(i18n.G("Something Not Working?"))
	heading.AddCssClass("title-2")
	heading.SetWrap(true)
	heading.SetJustify(gtk.JustifyCenterValue)
	u.reportDesc = gtk.NewLabel("")
	u.reportDesc.AddCssClass("dim-label")
	u.reportDesc.SetWrap(true)
	u.reportDesc.SetJustify(gtk.JustifyCenterValue)
	report := pill(i18n.G("Report a Problem"), func() { u.openCompose(publish.NewIssue) })
	report.AddCssClass("suggested-action")
	report.SetHalign(gtk.AlignCenterValue)
	report.SetMarginTop(6)
	hero := gtk.NewBox(gtk.OrientationVerticalValue, 6)
	hero.Append(&heading.Widget)
	hero.Append(&u.reportDesc.Widget)
	hero.Append(&report.Widget)

	// Secondary tasks: rows that say who each one is for, so Update isn't picked by someone it will refuse.
	existing := group(i18n.G("Already Reported?"))
	navRow := func(title, sub, icon, suffix string, cb func()) *adw.ActionRow {
		r := adw.NewActionRow()
		r.SetTitle(title)
		r.SetSubtitle(sub)
		r.SetActivatable(true)
		r.AddPrefix(&gtk.NewImageFromIconName(icon).Widget)
		r.AddSuffix(&gtk.NewImageFromIconName(suffix).Widget)
		r.ConnectActivated(hold(func(adw.ActionRow) { cb() }))
		return r
	}
	confirm := navRow(i18n.G("Confirm Existing Issue"), i18n.G("You have the same problem as an issue someone already reported"),
		"system-users-symbolic", "go-next-symbolic", func() { u.openCompose(publish.Confirm) })
	update := navRow(i18n.G("Update Your Issue"), i18n.G("Add a new system report to an issue you opened"),
		"document-edit-symbolic", "go-next-symbolic", func() { u.openCompose(publish.Update) })
	existing.Add(&confirm.Widget)
	existing.Add(&update.Widget)
	u.reportActs = []*gtk.Widget{&report.Widget, &confirm.Widget, &update.Widget}
	for _, w := range u.reportActs {
		w.SetSensitive(false) // until a snapshot tells us which repo this image reports to
	}

	hg := group(i18n.G("System Health"))
	u.health = gtk.NewListBox()
	u.health.SetSelectionMode(gtk.SelectionNoneValue)
	u.health.AddCssClass("boxed-list")
	hg.Add(&u.health.Widget)
	u.helpGroup = adw.NewPreferencesGroup()
	help := navRow(i18n.G("Get Help Fixing This"), i18n.G("Copies a summary of these problems and opens the assistant in Control Center"),
		"help-browser-symbolic", "adw-external-link-symbolic", u.troubleshoot) // opens another app, not a page
	u.helpGroup.Add(&help.Widget)
	u.helpGroup.SetVisible(false)

	dg := adw.NewPreferencesGroup() // unfinished reports belong next to the actions that create them
	dg.SetVisible(false)
	u.draftsGrp = dg
	u.draftsRow = adw.NewActionRow()
	u.draftsRow.SetTitle(i18n.G("Unfinished Reports"))
	u.draftsRow.SetActivatable(true)
	u.draftsRow.AddPrefix(&gtk.NewImageFromIconName("document-edit-symbolic").Widget)
	u.draftsCount = gtk.NewLabel("")
	u.draftsCount.AddCssClass("dim-label")
	u.draftsRow.AddSuffix(&u.draftsCount.Widget)
	u.draftsRow.AddSuffix(&gtk.NewImageFromIconName("go-next-symbolic").Widget)
	u.draftsRow.SetActionName("win.drafts")
	dg.Add(&u.draftsRow.Widget)

	// Details are for maintainers and the curious; collapsed so they don't bury the task.
	sys := group(i18n.G("This System"))
	u.sysDetails = adw.NewExpanderRow()
	u.sysDetails.SetTitle(i18n.G("System Details"))
	// TRANSLATORS: Image, Image ref, Tag and Digest describe the operating system's container image.
	for i, t := range []string{i18n.G("Name"), i18n.G("Version"), i18n.G("Image"), i18n.G("Kernel"), i18n.G("Architecture"), i18n.G("State"),
		i18n.G("Image ref"), i18n.G("Tag"), i18n.G("Digest"), i18n.G("CPU"), i18n.G("GPU"), i18n.G("GNOME")} {
		r := adw.NewActionRow()
		r.SetTitle(t)
		r.AddCssClass("property")
		u.sysRows[i] = r
		// A copy button on the right lets a user grab this fact (version, kernel, …)
		// without selecting text; its target value is set from the subtitle on load.
		copy := gtk.NewButtonFromIconName("edit-copy-symbolic")
		copy.SetTooltipText(copyFactTooltip(t))
		a11yLabel(&copy.Widget, copyFactTooltip(t))
		copy.SetValign(gtk.AlignCenterValue)
		copy.AddCssClass("flat")
		copy.SetActionName("win.copy-fact")
		u.sysCopyBtn[i] = copy
		r.AddSuffix(&copy.Widget)
		u.sysDetails.AddRow(&r.Widget)
	}
	sys.Add(&u.sysDetails.Widget)
	browse := navRow(i18n.G("Browse All Metrics"), i18n.G("Everything this report collects, searchable"), "view-list-symbolic", "go-next-symbolic",
		func() { u.nav.PushByTag("metrics") })
	sys.Add(&browse.Widget)

	u.spinner = adw.NewSpinner()
	loading := adw.NewStatusPage()
	loading.SetTitle(i18n.G("Collecting system report…"))
	loading.SetChild(&u.spinner.Widget)

	u.statusPage = adw.NewStatusPage()
	u.statusPage.SetIconName("dialog-warning-symbolic")
	u.statusPage.SetChild(&pill(i18n.G("Try Again"), u.refresh).Widget)

	u.ovStack = gtk.NewStack()
	u.ovStack.AddNamed(&loading.Widget, "loading")
	// Task first so reporting never needs a scroll; context follows, details last.
	u.ovStack.AddNamed(scrolled(&hero.Widget, &dg.Widget, &existing.Widget, &hg.Widget, &u.helpGroup.Widget, &sys.Widget), "ready")
	u.ovStack.AddNamed(&u.statusPage.Widget, "error")
	return u.page(i18n.G(appName), "overview", header, &u.ovStack.Widget)
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
		id := extras.Probe(ctx)
		post(func() {
			if gen != u.gen {
				log.Printf("ui: dropping stale snapshot gen %d", gen)
				return
			}
			u.refreshAct.SetEnabled(true)
			log.Printf("ui: snapshot gen %d in %s: %d system, %d user metrics, err=%v",
				gen, time.Since(start).Round(time.Millisecond), len(s.System.Metrics), len(s.User.Metrics), err)
			log.Printf("ui: identity ref=%q tag=%q digest=%q cpu=%q gpu=%q gnome=%q", id.Ref, id.Tag, id.Digest, id.CPU, id.GPU, id.GNOME)
			u.ident = id
			u.loaded(s, err)
		})
	}()
}

func (u *ui) loaded(s snapshot.Snapshot, err error) {
	if err != nil {
		u.snap = nil
		if errors.Is(err, snapshot.ErrNotInstalled) {
			u.statusPage.SetTitle(i18n.G("System reports are unavailable"))
			// TRANSLATORS: %s is the program name, systemd-report.
			u.statusPage.SetDescription(fmt.Sprintf(i18n.G("%s was not found. This system needs systemd 261 or newer with systemd-report"), snapshot.Binary))
		} else {
			u.statusPage.SetTitle(i18n.G("Couldn't collect a report"))
			u.statusPage.SetDescription(err.Error())
		}
		u.ovStack.SetVisibleChildName("error")
		u.announce(u.statusPage.GetTitle())
		return
	}
	u.snap = &s
	u.syncUserSession()
	u.sum = s.Summary()
	u.repoName = publish.Route(u.sum.ImageID)
	for _, w := range u.reportActs {
		w.SetSensitive(u.repoName != "")
	}
	if u.repoName == "" {
		// TRANSLATORS: %s is the image name, e.g. bluefin. Dakota and Utah are image names.
		u.reportDesc.SetText(fmt.Sprintf(i18n.G("Reporting isn't available for this image (%s). Only Dakota and Utah accept reports."), u.sum.ImageID))
	} else {
		// TRANSLATORS: %s is a GitHub repository, e.g. projectbluefin/dakota.
		u.reportDesc.SetText(fmt.Sprintf(i18n.G("Describe what went wrong and send it to the %s maintainers with a privacy-checked system report. You review everything before it's published."), u.repoName))
	}
	u.sysDetails.SetSubtitle(strings.TrimSpace(u.sum.Name + " " + u.sum.ImageVersion))
	for i, v := range []string{u.sum.Name, u.sum.ImageVersion, u.sum.ImageID, u.sum.Kernel, u.sum.Arch, u.sum.SystemState,
		u.ident.Ref, u.ident.Tag, u.ident.Digest, u.ident.CPU, u.ident.GPU, u.ident.GNOME} {
		known := v != ""
		if !known {
			v = i18n.G("Unknown")
		}
		u.sysRows[i].SetSubtitle(v)
		u.sysCopyBtn[i].SetVisible(known)
		u.sysCopyBtn[i].SetActionTargetValue(glib.NewVariantString(v))
	}

	u.syncHealth()

	rows := make([]string, 0, len(s.System.Metrics)+len(s.User.Metrics))
	row := func(prefix string, m snapshot.Metric) string {
		return prefix + familyName(m) + "\n" + m.String() // bind splits name / value onto two lines
	}
	for _, m := range s.System.Metrics {
		rows = append(rows, row("", m))
	}
	for _, m := range s.User.Metrics {
		rows = append(rows, row("user: ", m))
	}
	u.metricRows = rows
	u.metrics.Splice(0, u.metrics.GetNItems(), u.metricRows)
	u.syncResults()
	u.ovStack.SetVisibleChildName("ready")
	u.syncReview()
	n := len(u.sum.FailedUnits)
	u.announce(fmt.Sprintf(i18n.NG("System report collected: %d failed unit", "System report collected: %d failed units", n), n))
}

// announce tells assistive technologies about a change nothing else focuses.
func (u *ui) announce(msg string) {
	log.Printf("ui: announce: %s", msg)
	u.win.Announce(msg, gtk.AccessibleAnnouncementPriorityMediumValue)
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
		row := gtk.NewBox(gtk.OrientationHorizontalValue, 12)
		row.SetMarginStart(12)
		row.SetMarginEnd(12)
		row.SetMarginTop(6)
		row.SetMarginBottom(6)
		box := gtk.NewBox(gtk.OrientationVerticalValue, 6)
		box.SetHexpand(true)
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
		row.Append(&box.Widget)
		copy := gtk.NewButtonFromIconName("edit-copy-symbolic")
		copy.SetTooltipText(i18n.G("Copy Metric Value"))
		copy.SetValign(gtk.AlignCenterValue)
		copy.AddCssClass("flat")
		copy.SetActionName("win.copy-metric")
		row.Append(&copy.Widget)
		gtk.ListItemNewFromInternalPtr(p).SetChild(&row.Widget)
	}))
	f.ConnectBind(hold(func(_ gtk.SignalListItemFactory, p uintptr) {
		li := gtk.ListItemNewFromInternalPtr(p)
		s := gtk.StringObjectNewFromInternalPtr(li.GetItem().GoPointer()).GetString()
		name, val, _ := strings.Cut(s, "\n")
		row := li.GetChild()
		box := row.GetFirstChild()
		gtk.LabelNewFromInternalPtr(box.GetFirstChild().GoPointer()).SetText(name)
		gtk.LabelNewFromInternalPtr(box.GetLastChild().GoPointer()).SetText(val)
		box.SetTooltipText(metricTooltip(name, val, u.families))
		gtk.ButtonNewFromInternalPtr(row.GetLastChild().GoPointer()).SetActionTargetValue(glib.NewVariantString(val))
	}))
	lv := gtk.NewListView(gtk.NewNoSelection(model), &f.ListItemFactory)
	clamp := adw.NewClampScrollable() // same max width as the other pages
	clamp.SetChild(&lv.Widget)
	sw := gtk.NewScrolledWindow()
	sw.SetChild(&clamp.Widget)
	sw.SetVexpand(true)

	empty := adw.NewStatusPage()
	empty.SetIconName("edit-find-symbolic")
	results := gtk.NewStack()
	results.AddNamed(&sw.Widget, "results")
	results.AddNamed(&empty.Widget, "empty")
	results.SetVexpand(true)
	u.syncResults = func() {
		if model.GetNItems() > 0 {
			results.SetVisibleChildName("results")
			return
		}
		title, desc := emptyState(u.metrics.GetNItems(), u.search.GetText())
		empty.SetTitle(title)
		empty.SetDescription(desc)
		results.SetVisibleChildName("empty")
	}
	model.ConnectNotify(hold(func(gobject.Object, uintptr) { u.syncResults() }))

	search := gtk.NewSearchEntry()
	u.search = search
	search.SetPlaceholderText(i18n.G("Filter metrics"))
	a11yLabel(&search.Widget, i18n.G("Filter metrics")) // a placeholder is not an accessible name
	search.ConnectSearchChanged(hold(func(gtk.SearchEntry) {
		u.filter.SetSearch(search.GetText())
		u.syncResults()
	}))
	search.SetMarginTop(6)
	search.SetMarginBottom(6)
	search.SetMarginStart(12)
	search.SetMarginEnd(12)

	box := gtk.NewBox(gtk.OrientationVerticalValue, 0)
	sclamp := adw.NewClamp()
	sclamp.SetChild(&search.Widget)
	box.Append(&sclamp.Widget)
	box.Append(&results.Widget)
	u.syncResults()
	u.nav.Add(u.page(i18n.G("Metrics"), "metrics", nil, &box.Widget))
	u.nav.ConnectPushed(hold(func(adw.NavigationView) {
		if u.nav.GetVisiblePageTag() == "metrics" {
			u.loadMetricDescriptions()
		}
	}))
}

// copyFactTooltip is the copy button's label for a system-detail field, e.g. "Copy Version".
func copyFactTooltip(title string) string { return fmt.Sprintf(i18n.G("Copy %s"), title) }

// emptyState picks the metrics status page text: an empty collection wins over
// a non-matching query so a retained search never hides a failed collection.
func emptyState(total uint32, query string) (title, desc string) {
	if total > 0 && query != "" {
		return i18n.G("No Results Found"), i18n.G("Try a different search or clear the filter")
	}
	return i18n.G("No Metrics Available"), i18n.G("Refresh the snapshot and check the Overview for collection warnings")
}

// syncHealth rebuilds the overview health list from the snapshot's failed units
// and warnings. It runs after collection and again when the family descriptions
// arrive, so a describe failure joins the other collector warnings.
func (u *ui) syncHealth() {
	u.health.RemoveAll()
	add := func(title, sub, icon string) {
		r := adw.NewActionRow()
		r.SetTitle(title)
		r.SetSubtitle(sub)
		r.AddPrefix(&gtk.NewImageFromIconName(icon).Widget)
		u.health.Append(&r.Widget)
	}
	for _, f := range u.sum.FailedUnits {
		add(f, i18n.G("Failed unit"), "dialog-error-symbolic")
	}
	for _, w := range u.sum.Unreachable {
		add(w, i18n.G("Source unreachable"), "network-offline-symbolic")
	}
	for _, w := range u.sum.Warnings {
		if !slices.Contains(u.sum.Unreachable, w) { // Unreachable is a subset of Warnings
			add(w, i18n.G("Warning"), "dialog-warning-symbolic")
		}
	}
	problem := len(u.sum.FailedUnits)+len(u.sum.Warnings) > 0
	u.helpGroup.SetVisible(problem)
	if !problem {
		add(i18n.G("No problems found"), i18n.G("No failed units or warnings"), "object-select-symbolic")
	}
}

// loadMetricDescriptions fetches the family descriptions for both scopes the
// first time the Metrics page is shown, without blocking the page. A describe
// failure is surfaced as a warning alongside the collector warnings.
func (u *ui) loadMetricDescriptions() {
	if u.familiesReady {
		return
	}
	u.familiesReady = true
	go func() {
		fams := map[string]snapshot.Family{}
		var warns []string
		for _, scope := range []snapshot.Scope{snapshot.System, snapshot.User} {
			f, w, err := snapshot.Describe(context.Background(), snapshot.Run, scope)
			warns = append(warns, w...)
			if err != nil {
				warns = append(warns, fmt.Sprintf("%s describe: %v", scope, err)) // call() only warns on non-cannot-start paths
				continue
			}
			for name, fam := range f {
				if _, seen := fams[name]; !seen { // system and user share family names
					fams[name] = fam
				}
			}
		}
		post(func() {
			u.families = fams  // descriptions are snapshot-independent; store before the nil check
			if u.snap == nil { // a refresh replaced the snapshot while we were fetching
				return
			}
			u.snap.Warnings = append(u.snap.Warnings, warns...)
			u.sum = u.snap.Summary()
			u.syncHealth()
			u.reapplyMetrics()
		})
	}()
}

// reapplyMetrics re-runs the metrics bind so already-realised rows pick up the
// family descriptions now that they are available, without rebuilding the list.
// It runs once, the first time the Metrics page is shown, before the user has
// scrolled, so the list need not preserve its offset.
func (u *ui) reapplyMetrics() {
	if u.metrics == nil || len(u.metricRows) == 0 {
		return
	}
	u.metrics.Splice(0, u.metrics.GetNItems(), u.metricRows)
}

// troubleshoot hands the problem to ChairLift's Ask Bluefin flow, which owns the assistant's readiness and setup.
// Unit names can embed the username or hostname, so the copied prompt carries only image and counts;
// the assistant's read-only tools inspect the live services and logs themselves.
func (u *ui) troubleshoot() {
	var b strings.Builder
	fmt.Fprintf(&b, "Help troubleshoot this Bluefin problem from Report a Problem.\nImage: %s %s (%s)\n", u.sum.Name, u.sum.ImageVersion, u.sum.ImageID)
	fmt.Fprintf(&b, "Failed units: %d. Report warnings: %d.\n", len(u.sum.FailedUnits), len(u.sum.Warnings))
	b.WriteString("List the failed units, inspect their logs and status with your read-only tools, then suggest the next step.")
	u.win.GetClipboard().SetText(b.String())
	if u.dryRun {
		log.Print("dry-run: chairlift --ask-bluefin")
		u.toast(i18n.G("Problem summary copied"))
		return
	}
	go func() {
		cmd := exec.Command("chairlift", "--ask-bluefin")
		err := cmd.Start()
		if err == nil {
			go cmd.Wait() // reap it; ChairLift reports its own failures
		}
		post(func() {
			switch {
			case errors.Is(err, exec.ErrNotFound):
				u.showError(i18n.G("Couldn't Open Control Center"), i18n.G("Troubleshooting help needs Bluefin Control Center, which isn't installed"))
			case err != nil:
				u.showError(i18n.G("Couldn't Open Control Center"), err.Error())
			default:
				u.toast(i18n.G("Problem summary copied. Paste it into the assistant"))
			}
		})
	}()
}
