// Command changelog keeps the CHANGELOG entries of the changes that are not
// released yet as one file per change, in changelog.d, so that pull requests
// written in parallel never edit the same lines (#476). Every pull request
// used to add its entry at the top of the same list of CHANGELOG.md, and any
// two of them open together conflicted there.
//
// A fragment is changelog.d/<section>/<name>.md: the section is one of the
// sections of Keep a Changelog, in lower case (added, changed, deprecated,
// removed, fixed, security), and the name starts with the issue number, such
// as 440-verification-grant.md. Its text is the Markdown list item, or
// items, that goes under the section.
//
// Usage:
//
//	go run ./scripts/changelog check     validate every fragment
//	go run ./scripts/changelog render    print the fragments as the sections of the CHANGELOG
//	go run ./scripts/changelog collect   move them into [Unreleased] of CHANGELOG.md, at a release
//
// -dir and -file name the fragments directory and the CHANGELOG, for tests.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Section is a section of Keep a Changelog: its directory and its heading.
type Section struct {
	Dir, Heading string
}

// Sections are the sections, in the order a release lists them.
var Sections = []Section{
	{"added", "Added"}, {"changed", "Changed"}, {"deprecated", "Deprecated"},
	{"removed", "Removed"}, {"fixed", "Fixed"}, {"security", "Security"},
}

// namePattern is the name of a fragment: the issue number, when there is
// one, and a short slug, in lower case.
var namePattern = regexp.MustCompile(`^(\d+-)?[a-z0-9]+(-[a-z0-9]+)*\.md$`)

// Fragment is one entry of the CHANGELOG.
type Fragment struct {
	Section Section
	// Path is the file, and Name its base name.
	Path, Name string
	// Text is the list item or items, ending in one newline.
	Text string
}

// issue is the issue number at the start of the name, or a number beyond
// every issue for a fragment without one, so that those come last.
func (f Fragment) issue() int {
	number, _, found := strings.Cut(f.Name, "-")
	if n, err := strconv.Atoi(number); found && err == nil {
		return n
	}
	return int(^uint(0) >> 1)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("changelog", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "changelog.d", "The directory of the fragments")
	file := flags.String("file", "CHANGELOG.md", "The CHANGELOG")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: changelog [-dir changelog.d] [-file CHANGELOG.md] check|render|collect")
		return 2
	}
	fragments, problems := Read(*dir)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		fmt.Fprintf(stderr, "%d fragment problem(s); see %s/README.md\n", len(problems), *dir)
		return 1
	}
	switch flags.Arg(0) {
	case "check":
		fmt.Fprintf(stdout, "%d fragment(s) are valid.\n", len(fragments))
	case "render":
		fmt.Fprint(stdout, Render(fragments))
	case "collect":
		if err := Collect(*file, fragments); err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "Moved %d fragment(s) into %s.\n", len(fragments), *file)
	default:
		fmt.Fprintf(stderr, "unknown command %q: check, render, or collect\n", flags.Arg(0))
		return 2
	}
	return 0
}

// Read reads and validates the fragments of dir, sorted by section, issue,
// and name. It returns every problem it finds, not only the first. A
// missing directory has no fragments.
func Read(dir string) ([]Fragment, []string) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{err.Error()}
	}
	var fragments []Fragment
	var problems []string
	for _, e := range entries {
		name := e.Name()
		// Settings, such as .markdownlint.jsonc, are not fragments.
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !e.IsDir() {
			if name != "README.md" {
				problems = append(problems, fmt.Sprintf("%s: a fragment goes in a section directory, such as %s/added/", filepath.Join(dir, name), dir))
			}
			continue
		}
		i := slices.IndexFunc(Sections, func(s Section) bool { return s.Dir == name })
		if i < 0 {
			problems = append(problems, fmt.Sprintf("%s: not a section; the sections are %s", filepath.Join(dir, name), sectionDirs()))
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, f := range files {
			path := filepath.Join(dir, name, f.Name())
			if strings.HasPrefix(f.Name(), ".") {
				continue
			}
			fragment, problem := readFragment(Sections[i], path)
			if problem != "" {
				problems = append(problems, problem)
				continue
			}
			fragments = append(fragments, fragment)
		}
	}
	slices.SortFunc(fragments, func(a, b Fragment) int {
		if d := slices.Index(Sections, a.Section) - slices.Index(Sections, b.Section); d != 0 {
			return d
		}
		if d := a.issue() - b.issue(); d != 0 {
			if d < 0 {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return fragments, problems
}

func readFragment(s Section, path string) (Fragment, string) {
	name := filepath.Base(path)
	if !namePattern.MatchString(name) {
		return Fragment{}, fmt.Sprintf("%s: the name must be the issue number and a slug in lower case, such as 440-verification-grant.md", path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a fragment or the CHANGELOG of the repository, named on the command line.
	if err != nil {
		return Fragment{}, err.Error()
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	switch {
	case text == "":
		return Fragment{}, fmt.Sprintf("%s: empty", path)
	case !strings.HasPrefix(text, "- "):
		return Fragment{}, fmt.Sprintf("%s: a fragment is a list item, starting with \"- \"", path)
	}
	for n, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			return Fragment{}, fmt.Sprintf("%s:%d: no headings; the section comes from the directory", path, n+1)
		}
	}
	return Fragment{Section: s, Path: path, Name: name, Text: text + "\n"}, ""
}

func sectionDirs() string {
	dirs := make([]string, len(Sections))
	for i, s := range Sections {
		dirs[i] = s.Dir
	}
	return strings.Join(dirs, ", ")
}

// Render returns the fragments as the sections of the CHANGELOG, each
// heading followed by its entries, in the order of Sections.
func Render(fragments []Fragment) string {
	var b strings.Builder
	for _, s := range Sections {
		entries := entriesOf(fragments, s)
		if entries == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "### %s\n\n%s", s.Heading, entries)
	}
	return b.String()
}

func entriesOf(fragments []Fragment, s Section) string {
	var b strings.Builder
	for _, f := range fragments {
		if f.Section == s {
			b.WriteString(f.Text)
		}
	}
	return b.String()
}

// unreleased is the heading of the section that collect fills.
const unreleased = "## [Unreleased]"

// Collect moves the fragments into the [Unreleased] section of the
// CHANGELOG at path: the entries of each section go at the top of its
// subsection, which is created in the order of Sections when it is
// missing. Then it deletes the fragments. The CHANGELOG is written before
// any fragment is deleted, so a failure loses no entry.
func Collect(path string, fragments []Fragment) error {
	if len(fragments) == 0 {
		return nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a fragment or the CHANGELOG of the repository, named on the command line.
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	start := slices.Index(lines, unreleased)
	if start < 0 {
		return fmt.Errorf("%s has no %q section", path, unreleased)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	// A copy: inserting into a slice of lines would write over the
	// sections after it.
	block := slices.Clone(lines[start+1 : end])
	for _, s := range Sections {
		entries := entriesOf(fragments, s)
		if entries == "" {
			continue
		}
		block = insert(block, s, strings.Split(strings.TrimSuffix(entries, "\n"), "\n"))
	}
	out := slices.Concat(lines[:start+1], block, lines[end:])
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil { //nolint:gosec // G306: the CHANGELOG is a public file of the repository.
		return err
	}
	for _, f := range fragments {
		if err := os.Remove(f.Path); err != nil {
			return err
		}
	}
	return nil
}

// insert puts entries at the top of the subsection of s in block, the lines
// of [Unreleased] after its heading, creating the subsection before the
// first one that comes after s in Sections, or at the end.
func insert(block []string, s Section, entries []string) []string {
	heading := "### " + s.Heading
	if i := slices.Index(block, heading); i >= 0 {
		// The heading, a blank line, and then the entries.
		at := i + 1
		if at < len(block) && block[at] == "" {
			at++
		}
		return slices.Insert(block, at, entries...)
	}
	at := len(block)
	for _, later := range Sections[slices.Index(Sections, s)+1:] {
		if i := slices.Index(block, "### "+later.Heading); i >= 0 {
			at = i
			break
		}
	}
	if at == len(block) {
		// After the last entry: the blank lines that end the section stay
		// at its end.
		for at > 0 && block[at-1] == "" {
			at--
		}
	}
	section := append([]string{heading, ""}, entries...)
	if at > 0 && block[at-1] != "" {
		section = append([]string{""}, section...)
	}
	if at >= len(block) || block[at] != "" {
		section = append(section, "")
	}
	return slices.Insert(block, at, section...)
}
