# Contributing to caw

Thanks for taking the time to contribute. This project is small, opinionated, and
moves quickly — the guide below is the short path to a mergeable change.

## Prerequisites

Install the toolchain via [devbox](https://www.jetify.com/devbox) (recommended — pins
exact versions) or install `go 1.25.0`, `golangci-lint`, `dolt 2.1.4`, `python 3.12`,
`sqlfluff 4.2.2` manually.

## Setup

```sh
git clone https://github.com/ravencloak-org/caw.git
cd caw
devbox shell           # or install the toolchain manually (see Prerequisites)
make build && make test
```

## Conventional Commits

Commit messages must follow [Conventional Commits](https://www.conventionalcommits.org).
Changelog automation and release notes depend on it — `feat:`, `fix:`, `refactor:`,
`perf:`, `chore:`, `docs:`, `ci:`, `test:` with an optional `(scope)`.

## Schema changes

All database schema changes go through Dolt. See [`db/README.md`](./db/README.md)
for the workflow — never hand-edit the generated SQL.

## Pull requests

- Branch naming: `<type>/<slug>` (e.g. `feat/sse-backpressure`, `fix/lease-race`).
- CI must be green before review — no exceptions.
- Keep PRs scoped; split unrelated changes into separate PRs.

## Testing

`make test` runs unit + integration tests with the race detector enabled. Target
**80% patch coverage**; this is enforced by Codecov via [`codecov.yml`](./codecov.yml).
