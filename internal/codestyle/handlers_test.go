package codestyle

import (
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

// infrastructureImports are the packages that reach a database or a broker
// directly. A handler reaches them through the interfaces of its stores.
var infrastructureImports = map[string]bool{
	"database/sql": true,
	"github.com/mdastpak/crypto-exchange/internal/db":       true,
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq": true,
}

// TestHandlers_RunNoSQL checks the Go files of internal/handlers that are
// not tests (#48): a handler depends on interfaces of its stores, and runs
// no SQL. The SQL of a handler package that has no domain package of its
// own lives in its store.go, apart from the handler, behind an interface.
func TestHandlers_RunNoSQL(t *testing.T) {
	root := filepath.Join("..", "..")
	checked := 0
	err := filepath.WalkDir(filepath.Join(root, "internal", "handlers"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if d.Name() == "store.go" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		rel, _ := filepath.Rel(root, path)
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); infrastructureImports[p] {
				assert.Fail(t, "a handler reaches the infrastructure",
					"%s imports %s: use an interface of a store instead, with the SQL in the store", filepath.ToSlash(rel), p)
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Greater(t, checked, 30, "the walk found the handlers")
}
