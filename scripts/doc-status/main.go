// Command doc-status checks that the README's project status table agrees
// with the issues it links (#313). A row that says Implemented must link
// only closed issues; any other row (Partial, Planned, Foundation, ...) that
// links issues must link at least one open one, the work it waits for.
// Pull request links are not checked.
//
// It reads issue states from the GitHub API, so it runs in a scheduled
// workflow (.github/workflows/doc-status.yml), not in go test:
//
//	GITHUB_TOKEN=... go run ./scripts/doc-status -repo owner/name README.md
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Row is one row of the status table.
type Row struct {
	Capability string
	Status     string
	Issues     []int
}

var issueLink = regexp.MustCompile(`github\.com/([\w.-]+/[\w.-]+)/issues/(\d+)`)

// ParseStatusTable returns the rows of the table under the "## Project
// status" heading, with the issues of repo that each row links.
func ParseStatusTable(readme, repo string) ([]Row, error) {
	_, after, ok := strings.Cut(readme, "\n## Project status")
	if !ok {
		return nil, errors.New(`no "## Project status" section`)
	}
	var rows []Row
	header := 0
	for _, line := range strings.Split(after, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			break
		}
		if !strings.HasPrefix(line, "|") {
			if len(rows) > 0 {
				break
			}
			continue
		}
		// The header and its separator.
		if header < 2 {
			header++
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			return nil, fmt.Errorf("a status row has %d cells: %s", len(cells), line)
		}
		row := Row{Capability: strings.TrimSpace(cells[0]), Status: strings.TrimSpace(cells[1])}
		seen := map[int]bool{}
		for _, m := range issueLink.FindAllStringSubmatch(line, -1) {
			if !strings.EqualFold(m[1], repo) {
				continue
			}
			n, _ := strconv.Atoi(m[2])
			if !seen[n] {
				seen[n] = true
				row.Issues = append(row.Issues, n)
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, errors.New("the status table has no rows")
	}
	return rows, nil
}

// Check returns a problem for each row whose status the states of its
// issues contradict; open tells whether an issue is open.
func Check(rows []Row, open func(int) (bool, error)) ([]string, error) {
	var problems []string
	for _, row := range rows {
		if len(row.Issues) == 0 {
			continue
		}
		var opened, closed []string
		for _, n := range row.Issues {
			isOpen, err := open(n)
			if err != nil {
				return nil, err
			}
			if isOpen {
				opened = append(opened, "#"+strconv.Itoa(n))
			} else {
				closed = append(closed, "#"+strconv.Itoa(n))
			}
		}
		implemented := strings.EqualFold(row.Status, "Implemented")
		switch {
		case implemented && len(opened) > 0:
			problems = append(problems, fmt.Sprintf("%q says Implemented but links open %s", row.Capability, strings.Join(opened, ", ")))
		case !implemented && len(opened) == 0:
			problems = append(problems, fmt.Sprintf("%q says %s but every linked issue is closed (%s)", row.Capability, row.Status, strings.Join(closed, ", ")))
		}
	}
	return problems, nil
}

// githubStates reads issue states from the GitHub API, once per issue.
func githubStates(ctx context.Context, repo, token string) func(int) (bool, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	cache := map[int]bool{}
	return func(n int) (bool, error) {
		if v, ok := cache[n]; ok {
			return v, nil
		}
		//nolint:gosec // G704: the host is fixed; only the repository and number come from the operator.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://api.github.com/repos/%s/issues/%d", repo, n), nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req) //nolint:gosec // G704: see above.
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("issue #%d: GitHub answered %s", n, resp.Status)
		}
		var issue struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&issue); err != nil {
			return false, fmt.Errorf("issue #%d: %w", n, err)
		}
		cache[n] = issue.State == "open"
		return cache[n], nil
	}
}

func main() {
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "the repository whose issues are checked, owner/name")
	flag.Parse()
	path := flag.Arg(0)
	if path == "" {
		path = "README.md"
	}
	if err := run(path, *repo); err != nil {
		fmt.Fprintln(os.Stderr, "doc-status:", err)
		os.Exit(1)
	}
}

func run(path, repo string) error {
	if repo == "" {
		return errors.New("-repo or GITHUB_REPOSITORY is required")
	}
	readme, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the file to check.
	if err != nil {
		return err
	}
	rows, err := ParseStatusTable(string(readme), repo)
	if err != nil {
		return err
	}
	problems, err := Check(rows, githubStates(context.Background(), repo, os.Getenv("GITHUB_TOKEN")))
	if err != nil {
		return err
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d status rows disagree with their issues", len(problems))
	}
	fmt.Printf("%d status rows agree with their issues\n", len(rows))
	return nil
}
