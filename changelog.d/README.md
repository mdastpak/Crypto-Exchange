# Changelog fragments

The CHANGELOG entry of a change that is not released yet is a file of its own here, never an edit of [CHANGELOG.md](../CHANGELOG.md) ([#476](https://github.com/mdastpak/Crypto-Exchange/issues/476)). Pull requests written in parallel then never touch the same lines: each one adds its own file.

## Writing one

Add one file per change: `changelog.d/<section>/<issue>-<slug>.md`.

- **The section** is the directory: `added`, `changed`, `deprecated`, `removed`, `fixed`, or `security`, as in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
- **The name** starts with the issue number and a short slug in lower case, such as `440-verification-grant.md`. A change without an issue uses a slug alone, such as `docs-typo.md`. A pull request that changes two sections adds a file to each.
- **The text** is the Markdown list item that goes under the section, written as the entries of CHANGELOG.md are: a link to the issue, what changed, and what a developer or an operator must do. Nested items are fine. A fragment has no heading.

```markdown
- `admin verification grant` sets the verification level of a user ([#440](https://github.com/mdastpak/Crypto-Exchange/issues/440)).
  - `-reason` and `-operator` are required.
```

## Checking and reading them

- `go run ./scripts/changelog check` validates every fragment, as CI does.
- `go run ./scripts/changelog render` prints them as the sections of the CHANGELOG.

## At a release

`go run ./scripts/changelog collect` moves the fragments into the `[Unreleased]` section of CHANGELOG.md, each at the top of its section, and deletes them. One person runs it, in the pull request of the release, so it never races with other branches.
