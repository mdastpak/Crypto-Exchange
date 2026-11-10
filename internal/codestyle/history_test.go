package codestyle

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// history matches a comment that tells what the code used to do, or what a
// version that is gone needed, instead of what the code does. The history
// of a change belongs in its commit, its pull request, and the CHANGELOG
// (CONTRIBUTING.md, "Compatibility").
var history = regexp.MustCompile(`(?i)\bbefore #[0-9]+|\b(earlier|older|previous) releases?\b|\bearlier versions?\b|\breleases? before\b`)

// rolloutNote marks the comments that name the previous release on
// purpose: the ones that say what a rolling deployment requires.
const rolloutNote = "rolls out"

// TestComments_DescribeTheCodeNotItsHistory reads the comments of every
// Go file that is not a test.
func TestComments_DescribeTheCodeNotItsHistory(t *testing.T) {
	root := filepath.Join("..", "..")
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || name == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		checked++
		for _, group := range file.Comments {
			text := strings.Join(strings.Fields(group.Text()), " ")
			if strings.Contains(text, rolloutNote) {
				continue
			}
			if match := history.FindString(text); match != "" {
				rel, _ := filepath.Rel(root, path)
				assert.Fail(t, "a comment tells history", "%s:%d: %q: describe what the code does now",
					filepath.ToSlash(rel), fset.Position(group.Pos()).Line, match)
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Greater(t, checked, 100, "the walk found the repository")
}

// TestHistoryPattern checks the pattern itself.
func TestHistoryPattern(t *testing.T) {
	for _, text := range []string{
		"A volume created before #180 keeps its single role.",
		"seals the rows of earlier releases",
		"events of earlier versions have none",
		"as the previous release does",
		"Releases before the split wrote it here.",
	} {
		assert.True(t, history.MatchString(text), text)
	}
	for _, text := range []string{
		"The rate limit comes before the body limit.",
		"A session expires earlier when the limit is lowered.",
		"releases the lock before returning",
		"the version of the key that sealed it",
		"an older version of the key still opens it",
	} {
		assert.False(t, history.MatchString(text), text)
	}
}
