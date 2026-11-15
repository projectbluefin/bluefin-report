package i18n

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	gettext "github.com/snapcore/go-gettext"
)

// Release archives ship catalogs in locale/ beside the binary and installs
// put them in <prefix>/share/locale; if Dir stops finding either, the app
// silently shows English to everyone.
func TestDirSearchOrder(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	bin := filepath.Dir(exe)
	beside := filepath.Join(bin, "locale")
	share := filepath.Join(bin, "..", "share")
	prefixed := filepath.Join(share, "locale")
	for _, d := range []string{beside, prefixed} {
		if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
			t.Skipf("%s already exists beside the test binary", d)
		}
	}
	t.Cleanup(func() { LocaleDir = "" })

	if got := Dir(); got != gettext.DefaultLocaleDir {
		t.Errorf("no catalogs anywhere: Dir() = %q, want %q", got, gettext.DefaultLocaleDir)
	}

	if err := os.Mkdir(share, 0o755); err != nil {
		t.Skipf("cannot create %s: %v", share, err)
	}
	t.Cleanup(func() { os.RemoveAll(share) })
	if err := os.Mkdir(prefixed, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Dir(); got != prefixed {
		t.Errorf("installed under a prefix: Dir() = %q, want %q", got, prefixed)
	}

	// A file named locale is not a catalog directory.
	if err := os.WriteFile(beside, nil, 0o644); err != nil {
		t.Skipf("cannot create %s: %v", beside, err)
	}
	t.Cleanup(func() { os.RemoveAll(beside) })
	if got := Dir(); got != prefixed {
		t.Errorf("file named locale beside the binary: Dir() = %q, want %q", got, prefixed)
	}

	if err := os.Remove(beside); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(beside, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Dir(); got != beside {
		t.Errorf("release archive: Dir() = %q, want %q beside the binary", got, beside)
	}

	LocaleDir = "/opt/distro/locale"
	if got := Dir(); got != LocaleDir {
		t.Errorf("LocaleDir set at build time: Dir() = %q, want %q", got, LocaleDir)
	}
}
