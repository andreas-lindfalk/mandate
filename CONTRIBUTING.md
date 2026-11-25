# Contributing

Thank you for looking. pgmandate is small on purpose; please open an issue before a larger change, so we can agree on whether it belongs.

- The rules for code, tests and layout are in [AGENTS.md](AGENTS.md). They apply to people and to coding agents alike.
- `make check` regenerates the code and fails if it differs from what is committed, then runs the lint, the tests and the web build. CI runs the lint, the tests and the web build, but not the regeneration, so run `make check` before a pull request. It needs Go 1.27, Docker (for the tests), [buf](https://buf.build/docs/installation), [sqlc](https://docs.sqlc.dev/en/latest/overview/install.html), [golangci-lint](https://golangci-lint.run/welcome/install/) and, for the web app, Node 22 with pnpm 10.
- From 0.1.0 on, a migration that has been on `main` is never edited. Before 0.1.0 nothing is kept compatible. A change to the rules is a new numbered migration, with its tests.
- Every rule the database enforces has a test that bypasses the Go code and is refused by Postgres. A change to the schema comes with such a test.
- Security-relevant changes (schema, row level security, triggers, sign-in, MCP tools, deliveries) say so in the pull request.

## Licence of contributions

pgmandate is Apache-2.0. By opening a pull request you agree that your contribution is licensed under the same terms, and you certify the [Developer Certificate of Origin](https://developercertificate.org/) by signing off your commits (`git commit -s`).
