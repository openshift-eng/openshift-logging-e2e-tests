package testdata

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// resetExtraction forgets any previous extraction so the test starts clean,
// and removes what the test extracted.
func resetExtraction(t *testing.T) {
	t.Helper()
	reset := func() {
		if fixtureDir != "" {
			_ = os.RemoveAll(fixtureDir)
		}
		extractOnce = sync.Once{}
		fixtureDir = ""
	}
	reset()
	t.Cleanup(reset)
}

// TestFixturePathExtractsEverything checks that every embedded file is
// extracted with the same content, whichever path is requested first.
func TestFixturePathExtractsEverything(t *testing.T) {
	resetExtraction(t)

	file := FixturePath("logging", "subscription", "sub-template.yaml")
	root := FixturePath("logging")
	if want := filepath.Join(root, "subscription", "sub-template.yaml"); file != want {
		t.Errorf("file path = %q, want %q", file, want)
	}

	count := 0
	err := fs.WalkDir(fixtures, "logging", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		count++
		want, err := fixtures.ReadFile(name)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(fixtureDir, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("%s: not extracted: %v", name, err)
			return nil
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: extracted content differs from embedded content", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no fixtures embedded")
	}
}

func TestFixturePathPanicsOnBadPaths(t *testing.T) {
	tests := []struct {
		name    string
		elem    []string
		wantMsg string
	}{
		{"missing file", []string{"logging", "does-not-exist.yaml"}, "fixture not found"},
		{"testdata prefix", []string{"testdata", "logging"}, `drop the "testdata" prefix`},
		{"parent directory", []string{"logging", "..", ".."}, "fixture not found"},
		{"absolute", []string{"/logging"}, "fixture not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetExtraction(t)
			defer func() {
				r := recover()
				if msg, _ := r.(string); !strings.Contains(msg, tt.wantMsg) {
					t.Fatalf("panic = %v, want it to contain %q", r, tt.wantMsg)
				}
			}()
			FixturePath(tt.elem...)
		})
	}
}

// TestFixtureReferencesInTests catches references to fixtures that no longer
// exist (typos, renamed or deleted files) without needing a cluster. It parses
// the sibling test/e2e sources and checks every fixture path that is written
// out as string literals, either as testdata.FixturePath("logging", "x", ...)
// or as filepath.Join(loggingBaseDir, "x", ...), where loggingBaseDir is always
// testdata.FixturePath("logging"). Paths built from variables are not covered.
func TestFixtureReferencesInTests(t *testing.T) {
	const srcDir = ".."

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var direct, viaBaseDir int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(srcDir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			rel, kind := fixtureReference(call)
			if kind == refNone {
				return true
			}
			if kind == refDirect {
				direct++
			} else {
				viaBaseDir++
			}
			if _, err := fs.Stat(fixtures, rel); err != nil {
				t.Errorf("%s: references missing fixture %q", fset.Position(call.Pos()), rel)
			}
			return true
		})
	}

	// Guard against this test silently turning into a no-op after a refactor.
	if direct == 0 || viaBaseDir == 0 {
		t.Fatalf("expected to find fixture references of both kinds, got direct=%d viaBaseDir=%d", direct, viaBaseDir)
	}
	t.Logf("checked %d testdata.FixturePath calls and %d filepath.Join(loggingBaseDir, ...) calls", direct, viaBaseDir)
}

type refKind int

const (
	refNone refKind = iota
	refDirect
	refViaBaseDir
)

// fixtureReference returns the slash-separated fixture path referenced by call
// when every path element is a string literal.
func fixtureReference(call *ast.CallExpr) (string, refKind) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", refNone
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || call.Ellipsis != token.NoPos {
		return "", refNone
	}

	switch {
	case pkg.Name == "testdata" && sel.Sel.Name == "FixturePath":
		if lits, ok := stringLiterals(call.Args); ok && len(lits) > 0 {
			return path.Join(lits...), refDirect
		}
	case pkg.Name == "filepath" && sel.Sel.Name == "Join" && len(call.Args) > 1:
		if base, ok := call.Args[0].(*ast.Ident); ok && base.Name == "loggingBaseDir" {
			if lits, ok := stringLiterals(call.Args[1:]); ok {
				return path.Join(append([]string{"logging"}, lits...)...), refViaBaseDir
			}
		}
	}
	return "", refNone
}

func stringLiterals(exprs []ast.Expr) ([]string, bool) {
	out := make([]string, 0, len(exprs))
	for _, e := range exprs {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return nil, false
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
