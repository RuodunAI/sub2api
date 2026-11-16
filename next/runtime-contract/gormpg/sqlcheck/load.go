package sqlcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Package is a package as go list describes it.
type Package struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Deps       []string
	Export     string
	Error      *struct{ Err string }
}

// List runs go list -json in dir. The go command is the one that builds the
// tests, so its export data matches this go/types.
func List(dir string, args ...string) ([]Package, error) {
	cmd := exec.Command("go", append([]string{"list", "-json=ImportPath,Dir,GoFiles,Deps,Export,Error"}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %v: %v\n%s", args, err, stderr.String())
	}
	var pkgs []Package
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var p Package
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return pkgs, nil
		} else if err != nil {
			return nil, err
		}
		if p.Error != nil {
			return nil, fmt.Errorf("go list %s: %s", p.ImportPath, p.Error.Err)
		}
		pkgs = append(pkgs, p)
	}
}

// ModuleDir is the directory of module path, as the go command in dir
// resolves it.
func ModuleDir(dir, path string) (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", path)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go list -m %s: %w", path, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// WorkModules returns a ./dir/... pattern for each module of the go.work in
// work that lies under one of roots (relative to work): a pattern spans one
// module only.
func WorkModules(work string, roots ...string) ([]string, error) {
	cmd := exec.Command("go", "work", "edit", "-json")
	cmd.Dir = work
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go work edit -json: %w", err)
	}
	var w struct{ Use []struct{ DiskPath string } }
	if err := json.Unmarshal(out, &w); err != nil {
		return nil, err
	}
	var patterns []string
	for _, u := range w.Use {
		p := filepath.ToSlash(filepath.Clean(u.DiskPath))
		if slices.ContainsFunc(roots, func(r string) bool { return p == r || strings.HasPrefix(p, r+"/") }) {
			patterns = append(patterns, "./"+p+"/...")
		}
	}
	return patterns, nil
}

// Importer imports packages from the export data go list -export produced,
// so only the checked packages are type-checked from source.
func Importer(fset *token.FileSet, pkgs []Package) types.Importer {
	files := map[string]string{}
	for _, p := range pkgs {
		if p.Export != "" {
			files[p.ImportPath] = p.Export
		}
	}
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
}

// TypeCheck type-checks the files of package path, recording what Check
// needs.
func TypeCheck(fset *token.FileSet, imp types.Importer, path string, files []*ast.File) (*types.Info, error) {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	_, err := (&types.Config{Importer: imp}).Check(path, fset, files, info)
	return info, err
}

// ParseFiles parses the named files of dir, with their comments.
func ParseFiles(fset *token.FileSet, dir string, names []string) ([]*ast.File, error) {
	var files []*ast.File
	for _, name := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// GormUsers lists the packages of patterns, run in dir, that depend on GORM,
// and an importer for them.
func GormUsers(fset *token.FileSet, dir string, patterns ...string) ([]Package, types.Importer, error) {
	all, err := List(dir, patterns...)
	if err != nil {
		return nil, nil, err
	}
	var targets []Package
	for _, p := range all {
		if slices.Contains(p.Deps, gormPkg) {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return nil, nil, fmt.Errorf("no package of %v in %s uses GORM: the scan missed the tree", patterns, dir)
	}
	paths := make([]string, len(targets))
	for i, p := range targets {
		paths[i] = p.ImportPath
	}
	deps, err := List(dir, append([]string{"-export", "-deps"}, paths...)...)
	if err != nil {
		return nil, nil, err
	}
	return targets, Importer(fset, deps), nil
}

// Scan checks the non-test files of every package of patterns, run in dir,
// that depends on GORM, and returns the findings, then the entries of the
// policy that allowed or matched nothing (Unused).
func (c *Checker) Scan(dir string, patterns ...string) ([]string, error) {
	fset := token.NewFileSet()
	targets, imp, err := GormUsers(fset, dir, patterns...)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range targets {
		files, err := ParseFiles(fset, p.Dir, p.GoFiles)
		if err != nil {
			return nil, err
		}
		info, err := TypeCheck(fset, imp, p.ImportPath, files)
		if err != nil {
			return nil, fmt.Errorf("type-check %s: %w", p.ImportPath, err)
		}
		out = append(out, c.Check(fset, p.ImportPath, files, info)...)
	}
	for _, entry := range c.Unused() {
		out = append(out, entry+" is listed but matched nothing; drop its entry")
	}
	return out, nil
}
