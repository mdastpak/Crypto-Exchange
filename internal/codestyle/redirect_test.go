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

// TestHTTPClients_SetARedirectPolicy fails on an http.Client literal
// without CheckRedirect outside the tests (#554). Go's default policy sends
// a credential in a custom header, in the URL, or in a client certificate
// to whatever host a redirect names; each client chooses a policy of
// internal/redirect, or one of its own, on purpose.
func TestHTTPClients_SetARedirectPolicy(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			httpName := importName(file, "net/http")
			if httpName == "" {
				return nil
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isSelector(lit.Type, httpName, "Client") {
					return true
				}
				for _, elt := range lit.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "CheckRedirect" {
							return true
						}
					}
				}
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line))
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
	assert.Empty(t, offenders, "an http.Client without CheckRedirect follows redirects with its credentials; "+
		"set redirect.Refuse, or redirect.HTTPSOnly for a client without one")
}

// importName returns the name a file uses for the package at path, or ""
// when it does not import it.
func importName(file *ast.File, path string) string {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}
