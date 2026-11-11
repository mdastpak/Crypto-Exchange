package app

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadme_LayoutListsEveryPackage keeps the project layout of README.md
// in step with the code (#200): every directory under cmd/ and internal/
// must have a line of its own.
func TestReadme_LayoutListsEveryPackage(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	// A Windows checkout has CRLF line endings (core.autocrlf), and the
	// layout is matched line by line (#340).
	readme = bytes.ReplaceAll(readme, []byte("\r\n"), []byte("\n"))
	layout := regexp.MustCompile("(?s)## Project layout\n\n```text\n(.*?)```").FindSubmatch(readme)
	require.NotNil(t, layout, "README.md has no project layout block")

	for _, parent := range []string{"cmd", "internal"} {
		entries, err := os.ReadDir(filepath.Join("..", "..", parent))
		require.NoError(t, err)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(parent+"/"+entry.Name()+"/") + `\s`)
			assert.Regexp(t, line, string(layout[1]), "README.md does not list %s/%s/ in the project layout", parent, entry.Name())
		}
	}
}
