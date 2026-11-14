## Summary

<!-- What does this change do, and why? -->

## Related issues

<!-- Use a closing keyword, for example: Fixes #8 -->

Fixes #

## Type of change

- [ ] Bug fix
- [ ] Security fix
- [ ] New feature
- [ ] Enhancement
- [ ] Refactor (no behavior change)
- [ ] Tests
- [ ] Documentation
- [ ] Build, CI, or dependencies

## How it was tested

<!-- Commands you ran, new or updated tests, manual verification steps. -->

## Security review

<!--
Fill this in when the change touches a path of .github/CODEOWNERS:
internal/auth, internal/adminauth, internal/userauth, internal/passkey,
internal/passwordhash, internal/otp, internal/totp, internal/identity,
internal/signin, internal/keys, internal/rowseal, internal/audit,
internal/ledger, internal/destinations, internal/confirm,
internal/verification, internal/crypto, internal/platform,
internal/restrictions, internal/middleware, internal/db/migrations,
internal/db/roles.go, internal/config/validate.go, Dockerfile, .github.
Otherwise write "Not applicable".
-->

- [ ] **Threat:** what an attacker, a careless operator, or a broken dependency could do with this change, and what stops them.
- [ ] **Failure mode:** when a dependency (PostgreSQL, Redis, RabbitMQ, OpenBao, a provider) is down or slow, the change fails closed, or the reason it may fail open is written down.
- [ ] **Multi-node:** the change is correct with several instances at once, during a rolling deployment, and after a failover.
- [ ] **Migration:** the down migration works, and the up migration is safe on a database with data and with old instances still running.
- [ ] **Secrets and personal data:** none are logged, returned, or stored in clear; new ones are sealed or encrypted.
- [ ] **Size:** the change is under about 800 changed lines of non-generated code, or the description says why it cannot be split.

## Checklist

- [ ] `gofmt -l .` is empty, and `go vet ./...` and `go build ./...` pass.
- [ ] Tests are added or updated, and `go test -p 1 ./...` passes.
- [ ] New migrations include both up and down files, and no merged migration was modified.
- [ ] Documentation is updated when behavior or configuration changes: the README (its status table and architecture notes), `docs/architecture.md`, `docs/configuration.md`, the API reference, and the CHANGELOG, as they apply.
- [ ] No secrets, keys, or personal data are included.
- [ ] All code, comments, and text are in English.
