package ui

import (
	"bytes"
	"encoding/binary"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/projectbluefin/bluefin-report/internal/i18n"
)

// A catalog for the user's language replaces wrapped UI text at run time;
// text the catalog lacks stays in English.
func TestUITextUsesCatalog(t *testing.T) {
	english := t.TempDir()
	dir := t.TempDir()
	writeMO(t, filepath.Join(dir, "xx", "LC_MESSAGES", i18n.Domain+".mo"), map[string]string{
		"No Metrics Available": "Xx No Metrics",
	})
	t.Setenv("LANGUAGE", "xx")
	i18n.Init(dir)
	t.Cleanup(func() { i18n.Init(english) })

	title, desc := emptyState(0, "")
	if title != "Xx No Metrics" {
		t.Errorf("title = %q, want the catalog's translation", title)
	}
	if want := "Refresh the snapshot and check the Overview for collection warnings"; desc != want {
		t.Errorf("untranslated desc = %q, want English %q", desc, want)
	}
}

// writeMO writes a GNU .mo catalog (no hash table) holding msgs.
func writeMO(t *testing.T, path string, msgs map[string]string) {
	t.Helper()
	msgs[""] = "Content-Type: text/plain; charset=UTF-8\n"
	ids := slices.Sorted(maps.Keys(msgs)) // readers binary-search the msgids
	n := uint32(len(ids))
	var tables, strs bytes.Buffer
	offset := 28 + 16*n // header, then the msgid and msgstr tables
	for _, set := range []func(string) string{func(id string) string { return id }, func(id string) string { return msgs[id] }} {
		for _, id := range ids {
			s := set(id)
			binary.Write(&tables, binary.LittleEndian, [2]uint32{uint32(len(s)), offset + uint32(strs.Len())})
			strs.WriteString(s + "\x00")
		}
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [7]uint32{0x950412de, 0, n, 28, 28 + 8*n, 0, 0})
	b.Write(tables.Bytes())
	b.Write(strs.Bytes())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
