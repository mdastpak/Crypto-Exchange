package codestyle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const configImport = "github.com/mdastpak/crypto-exchange/internal/config"

// mayReadGlobals are the packages under internal/ that may read the global
// configuration (#48): the configuration itself, and the composition root
// of the server, which passes it to every other package. The commands under
// cmd/ are the composition roots of their own processes.
var mayReadGlobals = map[string]bool{"internal/config": true, "internal/app": true}

// globalsOfConfig returns the package-level names of internal/config that
// read or write its global state: its variables, and every function that
// uses one, or calls such a function.
func globalsOfConfig(t *testing.T, root string) map[string]bool {
	t.Helper()
	dir := filepath.Join(root, "internal", "config")
	entries, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	vars := map[string]bool{}
	funcs := map[string]*ast.FuncDecl{}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					for _, name := range spec.(*ast.ValueSpec).Names {
						vars[name.Name] = true
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && d.Body != nil {
					funcs[d.Name.Name] = d
				}
			}
		}
	}
	globals := map[string]bool{}
	for name := range vars {
		globals[name] = true
	}
	// A function that uses a global, directly or through another function,
	// reads the global state; repeat until nothing is added.
	for changed := true; changed; {
		changed = false
		for name, fn := range funcs {
			if globals[name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && globals[id.Name] && !globals[name] {
					globals[name], changed = true, true
				}
				return true
			})
		}
	}
	return globals
}

// TestPackages_ReadNoGlobalConfiguration checks every Go file under
// internal/ that is not a test: only internal/config and internal/app may
// use the global configuration, config.Config, or a function of the config
// package that reads it, such as config.MigrationsPath. Every other package
// receives the configuration from the composition root (#48), so that it
// can be built and tested with any configuration.
func TestPackages_ReadNoGlobalConfiguration(t *testing.T) {
	root := filepath.Join("..", "..")
	globals := globalsOfConfig(t, root)
	require.True(t, globals["Config"], "the global configuration is config.Config")
	require.True(t, globals["MigrationsPath"])

	checked := 0
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if mayReadGlobals[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		name := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == configImport {
				name = "config"
				if imp.Name != nil {
					name = imp.Name.Name
				}
			}
		}
		if name == "" {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == name && globals[sel.Sel.Name] {
				assert.Fail(t, "a package reads the global configuration",
					"%s:%d: %s.%s: take the configuration from the composition root (internal/app) instead",
					rel, fset.Position(sel.Pos()).Line, name, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	assert.Greater(t, checked, 100, "the walk found the code")
}
