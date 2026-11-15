package i18n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// `make pot` reads only the files in po/POTFILES, so a file that marks text
// for translation but is missing there ships that text untranslatable.
func TestPOTFILESListsEveryMarkedFile(t *testing.T) {
	const root = "../.."
	b, err := os.ReadFile(filepath.Join(root, "po", "POTFILES"))
	if err != nil {
		t.Fatal(err)
	}
	listed := strings.Fields(string(b))
	for _, f := range listed {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("po/POTFILES: %v", err)
		}
	}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "build" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if marks(t, path) && !slices.Contains(listed, filepath.ToSlash(rel)) {
			t.Errorf("%s marks text with i18n.G, NG or N but is not in po/POTFILES", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// marks reports whether the Go file at path calls i18n.G, i18n.NG or i18n.N.
func marks(t *testing.T, path string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "i18n" && slices.Contains([]string{"G", "NG", "N"}, sel.Sel.Name) {
				found = true
			}
		}
		return !found
	})
	return found
}
