# Contributing to Crypto Exchange

Thank you for your interest in contributing. This guide explains how work is organized and what a pull request needs before it can be merged.

By participating you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md). To report a security vulnerability, follow [SECURITY.md](SECURITY.md) instead of opening an issue.

## Language policy

All code, comments, identifiers, commit messages, issues, pull requests, and documentation must be written in clear English. Data that is legitimately multilingual, such as a currency's Persian display name in a test fixture, is the only exception.

## Finding work

Work is planned in milestones. Each milestone has a tracking epic with ordered sub-issues.

1. **[M0 - Stabilize current state](https://github.com/mdastpak/Crypto-Exchange/milestone/1)** ([#83](https://github.com/mdastpak/Crypto-Exchange/issues/83)) has the highest priority.
2. **[M1 - Foundation and hardening](https://github.com/mdastpak/Crypto-Exchange/milestone/2)** ([#84](https://github.com/mdastpak/Crypto-Exchange/issues/84)) comes next.
3. **M2 to M4** are the roadmap phases ([#85](https://github.com/mdastpak/Crypto-Exchange/issues/85), [#86](https://github.com/mdastpak/Crypto-Exchange/issues/86), [#87](https://github.com/mdastpak/Crypto-Exchange/issues/87)).

Within a milestone, work on P0 issues first, then P1, and so on. Comment on an issue before starting on it so that work is not duplicated. For anything that is not covered by an existing issue, open one first and discuss the approach.

## Labels and milestones

Every issue carries one label from each of these groups.

| Group | Labels | Meaning |
| --- | --- | --- |
| Type | `type: bug`, `type: security`, `type: feature`, `type: enhancement`, `type: refactor`, `type: chore`, `type: test`, `type: docs`, `type: epic` | The kind of work |
| Priority | `priority: P0 - critical` | Blocks core functionality or is directly exploitable. Fix first. |
| | `priority: P1 - high` | Required for a stable, secure baseline |
| | `priority: P2 - medium` | Should be addressed within the planned milestone |
| | `priority: P3 - low` | Nice to have |
| Feature | `feature: auth`, `feature: otp-login`, `feature: assets`, `feature: websocket`, `feature: messaging`, `feature: database`, `feature: rate-limiting`, `feature: http-platform`, `feature: config`, `feature: devops`, `feature: domain-model`, `feature: accounts`, `feature: kyc-aml`, `feature: custody`, `feature: price-engine`, `feature: otc`, `feature: market-data`, `feature: admin-api`, `feature: spot-trading`, `feature: derivatives`, `feature: observability` | The area of the product. An issue can have more than one. |

## Development setup

Follow [Getting started](README.md#getting-started) in the README. [docs/configuration.md](docs/configuration.md) describes every setting, and [docs/architecture.md](docs/architecture.md) explains the package layout.

## Workflow

1. Create a branch from the latest `main`. Use the pattern `<type>/<issue-number>-<short-description>`, for example `fix/8-users-contact-constraint` or `feat/58-price-aggregator`.
2. Make focused commits. Keep refactoring separate from behavior changes.
3. Open a pull request against `main` and fill in the template. Link the issue with a closing keyword, for example `Fixes #8`.
4. Address review feedback with new commits. Avoid force-pushing after a review has started.
5. A maintainer merges the pull request once the checks pass and it has been approved.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):

```text
<type>(<optional scope>): <imperative summary, at most 72 characters>

<optional body explaining what and why>

Fixes #<issue>
```

Allowed types: `feat`, `fix`, `security`, `perf`, `refactor`, `test`, `docs`, `build`, `ci`, and `chore`. Mark breaking changes with `!` after the type, or with a `BREAKING CHANGE:` footer.

Examples:

```text
fix(auth): reject refresh tokens on protected routes
feat(price): add Binance WebSocket provider
docs: document the OTP sign-in flow
```

## Coding standards

- Format the code with `gofmt`, and make sure `go vet ./...` and `golangci-lint run` (v2, configured in `.golangci.yml`) pass. CI checks all three on every pull request. A `//nolint` must name its linter and say why.
- Pass `context.Context` through every I/O call, and use the request context in handlers.
- Return errors with context (`fmt.Errorf("...: %w", err)`). Never return internal error details to clients.
- Use `shared.Decimal` for monetary values. Floating point is not allowed for money.
- Never log secrets, tokens, one-time codes, or unmasked personal data.
- Write comments in English that explain *why* something is done, not *what* the code does. A comment describes the code as it is: what it replaced, and which issue changed it, belong in the commit message, the pull request, and the CHANGELOG. `internal/codestyle` fails on a comment that says `before #123`, `earlier release`, or the like.
- Keep handlers thin. New business logic belongs in services with interface-based dependencies ([#48](https://github.com/mdastpak/Crypto-Exchange/issues/48)).
- A feature that only releases for Iran may have, such as an Iranian provider, exchange, or currency, adds itself to `regionFeatures` in `internal/config/region.go`, so that an international release refuses it ([Region of a release](docs/configuration.md#region-of-a-release)). Branch on `config.Config.Region()`, never on the raw setting.
- Paginate a list endpoint that can grow with `internal/pagination`: keyset pages with an opaque cursor, by a sort key with an index ([API conventions](docs/api.md#pagination)). Never use `OFFSET`, which gets slower the deeper the page and shifts items as rows are added. A cursor can be decoded, so its sort key holds only what the client may see: break ties with a public identifier, never an internal serial id.

## Compatibility

Nothing is deployed before version 1.0, so there is nobody to stay compatible with ([#312](https://github.com/mdastpak/Crypto-Exchange/issues/312)):

- **Before 1.0, add no compatibility code** unless a deployed consumer needs it and the pull request names that consumer. That covers a second path or name for a route, a second format of stored data that the code still reads, a setting kept under its old name, and a step that moves data out of a format no release wrote. Change the thing in place, and update its callers, its documentation, and the other repository (`crypto-blog`) in step.
- A change that an existing development database or configuration cannot follow is fine. Say in the CHANGELOG what a developer must do, for example recreate the database.
- Keeping two instances of *the same* build working together is not compatibility code: several nodes always run at once, and a rolling restart mixes them. Rotated keys, versioned rows, and migrations that an instance of the build before them survives stay required.
- **From 1.0 on, deprecations follow a schedule.** A route, a setting, or a format that a release shipped is first deprecated in a minor release, with a CHANGELOG entry, a `Deprecation` header or a start-up warning that names the replacement, and a removal version no earlier than the next major release. The code that reads the old form is removed in that version, together with its tests.

## Database migrations

- `1_baseline` is the whole schema of the first working version. Add new migrations as the next sequence number after it, with both `.up.sql` and `.down.sql` files in `internal/db/migrations/`.
- **Never modify a migration that has already been merged.** Write a new migration instead.
- A down migration must revert exactly what its up migration created, and nothing else.
- Verify the full cycle on a fresh database: up, down, then up again.
- Do not reference tables that a later migration creates. Add deferred foreign keys in the migration that creates the referenced table.

## Tests

- Add or update tests for every change. Bug fixes need a regression test.
- Prefer table-driven tests. Use `testify` (`assert` and `require`), as the existing tests do.
- Unit tests must not depend on external services.
- Integration tests need local PostgreSQL, Redis, and RabbitMQ instances. See [Testing](README.md#testing). Each package uses databases of its own, so `go test ./...` runs packages in parallel: give a new package its own `testdb_` database and the next Redis database from `internal/testutil/redistest`.
- Run the race detector for concurrent code: `go test -race ./...`. The race detector needs cgo, so it is not available on every development machine.
- Use the scenario catalog in [docs/testing/test-scenarios.md](docs/testing/test-scenarios.md) as the specification for expected behavior, and reference its scenario IDs (for example `A-27`) in test names or comments.

## Pull request checklist

- [ ] The pull request is linked to an issue and has a clear description.
- [ ] The CI checks pass: build, vet, formatting, tidy modules, lint, tests with the race detector, the vulnerability scan, and Markdown lint.
- [ ] `gofmt -l .` is empty, and `go vet ./...` and `go build ./...` pass.
- [ ] Tests are added or updated, and `go test ./...` passes.
- [ ] Migrations follow the rules above.
- [ ] Documentation (README, `docs/`, Swagger annotations) is updated when behavior or configuration changes, and the OpenAPI document is regenerated when a route or its annotations change (see [API documentation](#api-documentation)).
- [ ] No secrets, keys, or personal data are included.
- [ ] All text is in English.

## API documentation

The OpenAPI document in `docs/` (`docs.go`, `swagger.json`, `swagger.yaml`) is generated from the swag annotations of the handlers and of `main` in `cmd/server`. Do not edit it by hand. After adding or changing a route or its annotations, regenerate it with the swag version of `go.mod`:

```sh
go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/server/main.go -o docs --parseInternal
```

CI regenerates it and fails when it differs from the committed one, and `TestOpenAPI_DocumentsEveryRoute` fails when a route of the server is missing from it, or when it documents one that does not exist. Paths in the annotations are absolute, such as `/api/v1/public/currencies`.

The end-to-end tests of the server, in `internal/app`, also check every response they receive against the document ([#53](https://github.com/mdastpak/Crypto-Exchange/issues/53)): its status must be documented for its operation, or covered by a `default` response, and its body must match the schema, with no property that the schema lacks. A route that sends a new field, status, or type fails its tests until its annotations describe it. Routers that tests build must use `routeForTest`, which adds the check; `internal/testutil/openapitest` does the checking.

## Dependencies

Keep `go.mod` tidy (`go mod tidy`). Explain why each new dependency is needed in the pull request, and prefer the standard library and well-maintained modules. Dependabot pull requests are reviewed like any other change.

## License

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).
