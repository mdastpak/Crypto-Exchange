package codestyle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodeOwners_SecurityPaths keeps .github/CODEOWNERS true (#311): each
// path it lists exists, has an owner, and is named in the security review
// section of the pull request template, so that a renamed or moved package
// does not silently lose its review.
func TestCodeOwners_SecurityPaths(t *testing.T) {
	root := filepath.Join("..", "..")
	owners, err := os.ReadFile(filepath.Join(root, ".github", "CODEOWNERS"))
	require.NoError(t, err)
	template, err := os.ReadFile(filepath.Join(root, ".github", "PULL_REQUEST_TEMPLATE.md"))
	require.NoError(t, err)
	review := string(template)
	start := strings.Index(review, "## Security review")
	require.GreaterOrEqual(t, start, 0, "the template has a security review section")
	review = review[start:]
	if end := strings.Index(review[1:], "\n## "); end >= 0 {
		review = review[:end+1]
	}

	paths := 0
	for _, line := range strings.Split(string(owners), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		path := strings.Trim(fields[0], "/")
		paths++
		assert.Greater(t, len(fields), 1, "%s has an owner", fields[0])
		for _, owner := range fields[1:] {
			assert.True(t, strings.HasPrefix(owner, "@"), "%s: the owner %q is a user or a team", fields[0], owner)
		}
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
		assert.NoError(t, err, "%s is in CODEOWNERS but not in the repository", fields[0])
		assert.Contains(t, review, path, "the security review section of the template names %s", path)
	}
	assert.Greater(t, paths, 10, "the paths of CODEOWNERS are read")
}
