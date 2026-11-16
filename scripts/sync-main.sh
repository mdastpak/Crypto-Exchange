#!/usr/bin/env bash
# Brings the current branch up to date with main before a merge, as
# CONTRIBUTING.md asks ("Working in parallel", #476).
#
# It merges origin/main. A conflict in the generated OpenAPI document
# (docs/docs.go, docs/swagger.json, docs/swagger.yaml) is resolved by
# generating it again from the merged code; any other conflict stops the
# script, for a person to resolve and run it again. Then it generates the
# document again anyway, since two sides that merge without a conflict can
# still disagree, and commits it when it changed. Last, it runs the checks
# that parallel branches break most: the numbers of the migration files and
# the changelog fragments.
#
# Usage: scripts/sync-main.sh   (from anywhere in the repository, with a clean
# working tree)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
generated=(docs/docs.go docs/swagger.json docs/swagger.yaml)
swag="github.com/swaggo/swag/cmd/swag@$(go list -m -f '{{.Version}}' github.com/swaggo/swag)"

regenerate() {
  go run "$swag" init -g cmd/server/main.go -o docs --parseInternal >/dev/null
}

if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "Commit or stash your changes first." >&2
  exit 1
fi

git fetch origin main
if ! git merge --no-edit origin/main; then
  others=$(git diff --name-only --diff-filter=U | grep -vxF -f <(printf '%s\n' "${generated[@]}") || true)
  if [ -n "$others" ]; then
    # The generated files take main's side for now: the next run, after the
    # merge is committed, generates them from the merged code.
    for f in "${generated[@]}"; do
      if git diff --name-only --diff-filter=U | grep -qxF "$f"; then
        git checkout --theirs -- "$f"
        git add -- "$f"
      fi
    done
    echo "These files conflict and need a person. Resolve them, commit the merge, and run this script again:" >&2
    echo "$others" >&2
    exit 1
  fi
  echo "Only the generated OpenAPI document conflicts: generating it from the merged code."
  regenerate
  git add -- "${generated[@]}"
  git commit --no-edit
fi

regenerate
if ! git diff --quiet -- "${generated[@]}"; then
  git add -- "${generated[@]}"
  git commit -m "Generate the OpenAPI document again after merging main"
fi

# The migration numbers and spelling, read from the files: no database is
# needed. The full suite runs in CI.
go test ./internal/db -run 'TestMigrations_ComeInPairs|TestMigrations_SpellTimestampTZ' -count=1
go run ./scripts/changelog check

echo "Up to date with origin/main: $(git rev-parse --short origin/main)."
