package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const readme = `# Title

## Project status

| Capability | Status | Notes |
| --- | --- | --- |
| Sign-in | Implemented | Done ([#1](https://github.com/o/r/issues/1), [PR #9](https://github.com/o/r/pull/9)). |
| Sockets | Partial | Channels in [#2](https://github.com/o/r/issues/2) and [#3](https://github.com/o/r/issues/3). |
| Ledger | Planned | [epic](https://github.com/o/r/issues/4), elsewhere [#5](https://github.com/other/r/issues/5) |
| Audit | Implemented | No links. |

## Architecture

| Not | A | Status |
| --- | --- | --- |
| x | y | [#6](https://github.com/o/r/issues/6) |
`

func TestParseStatusTable(t *testing.T) {
	rows, err := ParseStatusTable(readme, "o/r")
	require.NoError(t, err)
	assert.Equal(t, []Row{
		{Capability: "Sign-in", Status: "Implemented", Issues: []int{1}},
		{Capability: "Sockets", Status: "Partial", Issues: []int{2, 3}},
		{Capability: "Ledger", Status: "Planned", Issues: []int{4}},
		{Capability: "Audit", Status: "Implemented"},
	}, rows)

	_, err = ParseStatusTable("# No table\n", "o/r")
	assert.Error(t, err)
}

func TestCheck(t *testing.T) {
	rows, err := ParseStatusTable(readme, "o/r")
	require.NoError(t, err)
	states := func(open ...int) func(int) (bool, error) {
		return func(n int) (bool, error) {
			for _, o := range open {
				if o == n {
					return true, nil
				}
			}
			return false, nil
		}
	}

	problems, err := Check(rows, states(3, 4))
	require.NoError(t, err)
	assert.Empty(t, problems, "Implemented links closed issues; Partial and Planned each link an open one")

	problems, err = Check(rows, states(1, 4))
	require.NoError(t, err)
	assert.Equal(t, []string{
		`"Sign-in" says Implemented but links open #1`,
		`"Sockets" says Partial but every linked issue is closed (#2, #3)`,
	}, problems)
}

// TestParseStatusTable_TheReadme checks that the repository's README parses,
// so that the scheduled check does not fail on the format.
func TestParseStatusTable_TheReadme(t *testing.T) {
	data, err := os.ReadFile("../../README.md")
	require.NoError(t, err)
	rows, err := ParseStatusTable(string(data), "mdastpak/Crypto-Exchange")
	require.NoError(t, err)
	assert.Greater(t, len(rows), 10)
}
