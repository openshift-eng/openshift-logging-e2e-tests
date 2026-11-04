// list-labels statically lists the ginkgo labels used by the e2e test suites.
//
// It mirrors the approach of `ginkgo labels` (an AST scan of Label(...) calls,
// no tests are run or compiled) but additionally resolves label constants and
// label-builder helpers defined in internal/labels, e.g.
//
//	Label(labels.ComponentCLO, labels.Jira("LOG-9876"))
//
// resolves to ["component:clo", "jira:LOG-9876"].
//
// With -names "<label...>" it instead reads the JSON output of
// `openshift-logging-e2e-tests-tests-ext list -o json` from stdin and prints
// the name of every spec carrying all the given labels (AND semantics).
//
// Usage: go run ./internal/tools/list-labels [-grep substring] [-names "<l1> <l2>..."] [dirs...]
// With no dirs it scans ./test/...
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const labelsPkgPath = "github.com/openshift/openshift-logging-e2e-tests/internal/labels"

func main() {
	grep := flag.String("grep", "", "only list labels containing this substring")
	names := flag.String("names", "", "read `list -o json` output from stdin and print the names of specs carrying all these space-separated labels")
	flag.Parse()

	if *names != "" {
		if err := selectNames(*names); err != nil {
			fmt.Fprintf(os.Stderr, "selecting spec names: %v\n", err)
			os.Exit(1)
		}
		return
	}

	dirs := flag.Args()
	if len(dirs) == 0 {
		dirs = []string{"./test/..."}
	}

	// Resolve label constants and prefix helpers from internal/labels.
	consts, prefixFuncs, err := parseLabelsPackage("./internal/labels")
	if err != nil {
		fmt.Fprintf(os.Stderr, "parsing internal/labels: %v\n", err)
		os.Exit(1)
	}

	labels := map[string]bool{}
	for _, pattern := range dirs {
		recursive := strings.HasSuffix(pattern, "/...")
		root := strings.TrimSuffix(pattern, "/...")
		if recursive {
			// walk every directory under root
			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() && isDirWithGo(path) {
					scanDir(path, consts, prefixFuncs, labels)
				}
				return nil
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "scanning %s: %v\n", root, err)
				os.Exit(1)
			}
			continue
		}
		if isDirWithGo(root) {
			scanDir(root, consts, prefixFuncs, labels)
		}
	}

	out := make([]string, 0, len(labels))
	for l := range labels {
		if *grep == "" || strings.Contains(l, *grep) {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	for _, l := range out {
		fmt.Println(l)
	}
}

func isDirWithGo(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// scanDir parses one package directory and collects labels from Label(...) calls.
func scanDir(dir string, consts map[string]string, prefixFuncs map[string]string, out map[string]bool) {
	fset := token.NewFileSet()
	files, err := parseDirFiles(fset, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping %s: %v\n", dir, err)
		return
	}
	for _, file := range files {
		scanFile(file, consts, prefixFuncs, out)
	}
}

// parseDirFiles parses every .go file in dir. It replaces the deprecated
// parser.ParseDir: this tool wants all files regardless of build tags, so
// per-file parsing is equivalent.
func parseDirFiles(fset *token.FileSet, dir string) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

func scanFile(file *ast.File, consts map[string]string, prefixFuncs map[string]string, out map[string]bool) {
	// Map local package names to the internal/labels import, honoring aliases.
	labelsPkgNames := map[string]bool{}
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path != labelsPkgPath {
			continue
		}
		name := filepath.Base(path) // "labels"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		labelsPkgNames[name] = true
	}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isLabelCall(call.Fun) {
			return true
		}
		for _, arg := range call.Args {
			for _, label := range resolveLabelExpr(arg, labelsPkgNames, consts, prefixFuncs) {
				out[label] = true
			}
		}
		return true
	})
}

func isLabelCall(fun ast.Expr) bool {
	switch e := fun.(type) {
	case *ast.Ident:
		return e.Name == "Label"
	case *ast.SelectorExpr:
		return e.Sel.Name == "Label"
	}
	return false
}

// resolveLabelExpr turns a Label(...) argument into the label strings it denotes.
func resolveLabelExpr(expr ast.Expr, labelsPkgNames map[string]bool, consts map[string]string, prefixFuncs map[string]string) []string {
	switch e := expr.(type) {
	case *ast.BasicLit: // "literal"
		if e.Kind == token.STRING {
			if s, err := strconv.Unquote(e.Value); err == nil {
				return []string{s}
			}
		}
	case *ast.Ident: // Const
		if v, ok := consts[e.Name]; ok {
			return []string{v}
		}
	case *ast.SelectorExpr: // labels.Const
		if pkg, ok := e.X.(*ast.Ident); ok && labelsPkgNames[pkg.Name] {
			if v, ok := consts[e.Sel.Name]; ok {
				return []string{v}
			}
		}
	case *ast.CallExpr: // labels.Jira("KEY")
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || !labelsPkgNames[pkg.Name] {
			return nil
		}
		prefix, ok := prefixFuncs[sel.Sel.Name]
		if !ok || len(e.Args) != 1 {
			return nil
		}
		if key, ok := stringArg(e.Args[0]); ok {
			return []string{prefix + key}
		}
	}
	return nil
}

func stringArg(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// parseLabelsPackage extracts string constants and prefix-builder helpers
// (funcs of the form `func X(key string) string { return "prefix" + key }`)
// from the internal/labels package.
func parseLabelsPackage(dir string) (consts map[string]string, prefixFuncs map[string]string, err error) {
	consts = map[string]string{}
	prefixFuncs = map[string]string{}
	fset := token.NewFileSet()
	files, err := parseDirFiles(fset, dir)
	if err != nil {
		return nil, nil, err
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl: // const Name = "value"
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
						continue
					}
					if v, ok := stringArg(vs.Values[0]); ok {
						consts[vs.Names[0].Name] = v
					}
				}
			case *ast.FuncDecl: // func X(key string) string { return "prefix" + key }
				if d.Recv != nil || d.Type.Params == nil || len(d.Type.Params.List) != 1 ||
					d.Type.Results == nil || len(d.Type.Results.List) != 1 ||
					len(d.Body.List) != 1 {
					continue
				}
				ret, ok := d.Body.List[0].(*ast.ReturnStmt)
				if !ok || len(ret.Results) != 1 {
					continue
				}
				bin, ok := ret.Results[0].(*ast.BinaryExpr)
				if !ok || bin.Op != token.ADD {
					continue
				}
				prefix, ok1 := stringArg(bin.X)
				param, ok2 := bin.Y.(*ast.Ident)
				ok3 := len(d.Type.Params.List[0].Names) == 1 && d.Type.Params.List[0].Names[0].Name == param.Name
				if ok1 && ok2 && ok3 {
					prefixFuncs[d.Name.Name] = prefix
				}
			}
		}
	}
	return consts, prefixFuncs, nil
}

// selectNames reads the extension binary's `list -o json` output from stdin
// and prints the name of every spec whose labels include all the given
// space-separated labels.
func selectNames(wanted string) error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	// the binary may log to stdout before the JSON; trim to the first array
	start := strings.Index(string(raw), "[")
	if start < 0 {
		return fmt.Errorf("no JSON array found on stdin")
	}
	var specs []struct {
		Name   string                     `json:"name"`
		Labels map[string]json.RawMessage `json:"labels"`
	}
	if err := json.Unmarshal(raw[start:], &specs); err != nil {
		return fmt.Errorf("parsing specs JSON: %w", err)
	}
	wantedSet := map[string]bool{}
	for l := range strings.FieldsSeq(wanted) {
		wantedSet[l] = true
	}
	for _, spec := range specs {
		has := true
		for l := range wantedSet {
			if _, ok := spec.Labels[l]; !ok {
				has = false
				break
			}
		}
		if has {
			fmt.Println(spec.Name)
		}
	}
	return nil
}
