// Package i18n translates user-visible text with GNU gettext catalogs,
// <dir>/<lang>/LC_MESSAGES/bluefin-report.mo, picking the language from
// LANGUAGE, LC_ALL, LC_MESSAGES and LANG as libintl does.
//
// Mark strings with G, NG or N only: `make pot` extracts exactly those calls
// from the files listed in po/POTFILES.
package i18n

import (
	"os"
	"path/filepath"

	gettext "github.com/snapcore/go-gettext"
)

// Domain names the catalogs: bluefin-report.mo and po/bluefin-report.pot.
const Domain = "bluefin-report"

// LocaleDir, when set at build time with -ldflags -X (distribution packages),
// is the only directory searched for catalogs.
var LocaleDir string

// The zero Catalog returns every message untranslated.
var catalog gettext.Catalog

// Dir is where catalogs are installed: LocaleDir when set; otherwise "locale"
// beside the executable (release archive, make build) or "share/locale" in
// its install prefix; otherwise /usr/share/locale.
func Dir() string {
	if LocaleDir != "" {
		return LocaleDir
	}
	if exe, err := os.Executable(); err == nil {
		bin := filepath.Dir(exe)
		for _, d := range []string{filepath.Join(bin, "locale"), filepath.Join(bin, "..", "share", "locale")} {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				return d
			}
		}
	}
	return gettext.DefaultLocaleDir
}

// Init loads the catalogs for the user's languages from dir. Messages with no
// translation, or with no catalog at all, stay in English. Call it before the
// first G or NG.
func Init(dir string) {
	catalog = (&gettext.TextDomain{Name: Domain, LocaleDir: dir}).UserLocale()
}

// G returns the translation of msgid (gettext; _() in C).
func G(msgid string) string { return catalog.Gettext(msgid) }

// NG returns the translation of msgid or plural that fits n (ngettext).
// Both forms must use the same format verbs, e.g. "%d file" and "%d files".
func NG(msgid, plural string, n int) string {
	return catalog.NGettext(msgid, plural, uint32(max(n, 0)))
}

// N marks msgid for translation without translating it (N_() in C), for
// text stored now and passed through G where it is shown.
func N(msgid string) string { return msgid }
