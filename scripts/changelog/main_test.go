package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// write writes the files of a fragments directory, by path, under dir.
func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	}
}

func TestRead_SortsBySectionIssueAndName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"README.md":                      "# Fragments\n",
		"fixed/9-a-fix.md":               "- A fix.\n",
		"added/440-grant.md":             "- The grant.\n  - With a detail.\n",
		"added/76-orders.md":             "- Orders.\n",
		"added/docs-only.md":             "- A change without an issue.\n",
		"security/12-seal.md":            "- A seal.\r\n",
		"changed/.gitkeep":               "",
		"deprecated/300-old-setting.md":  "- An old setting.\n",
		"added/440-grant-second-step.md": "- The second step.\n",
	})
	fragments, problems := Read(dir)
	require.Empty(t, problems)
	var names []string
	for _, f := range fragments {
		names = append(names, f.Section.Dir+"/"+f.Name)
	}
	assert.Equal(t, []string{
		"added/76-orders.md", "added/440-grant-second-step.md", "added/440-grant.md", "added/docs-only.md",
		"deprecated/300-old-setting.md", "fixed/9-a-fix.md", "security/12-seal.md",
	}, names)
	assert.Equal(t, "- A seal.\n", fragments[6].Text, "CRLF and the trailing space are normalized")
}

func TestRead_RefusesWhatIsNotAFragment(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"added/Upper-Case.md":    "- An entry.\n",
		"added/440_grant.md":     "- An entry.\n",
		"added/440-grant.txt":    "- An entry.\n",
		"added/440-empty.md":     " \n",
		"added/440-prose.md":     "An entry without a list.\n",
		"added/440-heading.md":   "- An entry.\n## A heading\n",
		"feature/440-section.md": "- An entry.\n",
		"440-top-level.md":       "- An entry.\n",
		"fixed/9-good.md":        "- A good one.\n",
	})
	fragments, problems := Read(dir)
	require.Len(t, fragments, 1, "the good one is read")
	all := strings.Join(problems, "\n")
	for _, want := range []string{"Upper-Case.md", "440_grant.md", "440-grant.txt", "440-empty.md: empty",
		"440-prose.md: a fragment is a list item", "440-heading.md:2: no headings", "feature: not a section",
		"440-top-level.md: a fragment goes in a section directory"} {
		assert.Contains(t, all, want)
	}
	assert.Len(t, problems, 8)

	none, problems := Read(filepath.Join(dir, "missing"))
	assert.Empty(t, none)
	assert.Empty(t, problems, "no directory is no fragment")
}

func TestRender_ListsTheSectionsInOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"security/2-b.md": "- Security entry.\n",
		"added/1-a.md":    "- Added entry.\n  - Detail.\n",
		"added/3-c.md":    "- Another added entry.\n",
	})
	fragments, problems := Read(dir)
	require.Empty(t, problems)
	assert.Equal(t, "### Added\n\n- Added entry.\n  - Detail.\n- Another added entry.\n\n### Security\n\n- Security entry.\n",
		Render(fragments))
	assert.Empty(t, Render(nil))
}

const changelog = `# Changelog

Intro.

## [Unreleased]

New entries are in changelog.d.

### Added

- Older added entry.

### Fixed

- Older fix.

## 2026-09-16

### Added

- Released entry.
`

func TestCollect_MovesTheFragmentsIntoUnreleased(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "CHANGELOG.md")
	require.NoError(t, os.WriteFile(file, []byte(changelog), 0o600))
	fragments := filepath.Join(dir, "changelog.d")
	write(t, fragments, map[string]string{
		"README.md":         "# Fragments\n",
		"added/440-a.md":    "- New added entry.\n  - Detail.\n",
		"changed/441-b.md":  "- New changed entry.\n",
		"security/442-c.md": "- New security entry.\n",
	})
	read, problems := Read(fragments)
	require.Empty(t, problems)
	require.NoError(t, Collect(file, read))

	got, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, `# Changelog

Intro.

## [Unreleased]

New entries are in changelog.d.

### Added

- New added entry.
  - Detail.
- Older added entry.

### Changed

- New changed entry.

### Fixed

- Older fix.

### Security

- New security entry.

## 2026-09-16

### Added

- Released entry.
`, string(got), "new entries on top; missing sections in the order of Keep a Changelog; released ones untouched")

	left, problems := Read(fragments)
	assert.Empty(t, problems)
	assert.Empty(t, left, "the fragments are deleted")
	_, err = os.Stat(filepath.Join(fragments, "README.md"))
	assert.NoError(t, err, "the README stays")
}

func TestCollect_RefusesAChangelogWithoutUnreleased(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "CHANGELOG.md")
	require.NoError(t, os.WriteFile(file, []byte("# Changelog\n\n## 2026-09-16\n"), 0o600))
	write(t, dir, map[string]string{"changelog.d/added/1-a.md": "- An entry.\n"})
	read, _ := Read(filepath.Join(dir, "changelog.d"))
	require.Error(t, Collect(file, read))
	_, err := os.Stat(filepath.Join(dir, "changelog.d", "added", "1-a.md"))
	assert.NoError(t, err, "nothing is deleted when the CHANGELOG cannot take the entries")
}

func TestRun_ChecksAndReportsEveryProblem(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"added/1-a.md": "- An entry.\n"})
	var stdout, stderr bytes.Buffer
	assert.Equal(t, 0, run([]string{"-dir", dir, "check"}, &stdout, &stderr), stderr.String())
	assert.Contains(t, stdout.String(), "1 fragment(s) are valid")

	write(t, dir, map[string]string{"added/Bad.md": "x\n", "fixed/2-b.md": "no list\n"})
	stdout.Reset()
	stderr.Reset()
	assert.Equal(t, 1, run([]string{"-dir", dir, "check"}, &stdout, &stderr))
	assert.Contains(t, stderr.String(), "2 fragment problem(s)")

	assert.Equal(t, 2, run([]string{"-dir", dir}, &stdout, &stderr), "a command is required")
	assert.Equal(t, 2, run([]string{"-dir", t.TempDir(), "publish"}, &stdout, &stderr))
}
