# pgmandate

Shared context for the people and the AI agents (Claude Code, Codex and others) working in this repository. Everything that matters for the project lives here or in `docs/`, not in local notes or in one agent's memory. If existing code contradicts this file, the file wins.

Personal instructions go in `AGENTS.local.md`, which git ignores. It overrides this file only where the two collide.

## What pgmandate is

pgmandate is a reference implementation of one idea: **an AI agent holds no credentials; it proposes, and a person or a person's standing permission decides.**

An agent proposes an *action* with a payload. Either a person approves, edits or rejects the proposal, or a *mandate* that a person granted allows it at once. A mandate is data, not prompt text: which agent, which actions, up to which risk, within which budget per month, resting on which adopted policy document, until when. The decided proposal is delivered, signed, to the system that carries it out, and the answer comes back to the agent. Every proposal, decision, delivery and refusal is a row in a hash linked log: the evidence.

pgmandate runs as a service: one binary with one Postgres, locally, hosted or on-premise. Agents connect over MCP, systems receive decisions through signed webhooks, and people sign in to a web UI. Embedding the Go library is an option for a host that shares the database.

It is not an agent framework, an LLM gateway or an orchestrator, and it is small on purpose.

## Fixed principles

- **Opt-in in every layer.** Nothing is allowed without a person unless the action is defined as delegable, a person has granted a live mandate that covers the agent and the action, the proposal's risk is within the mandate, nothing is flagged, and the month's budget has room. Any of these missing leaves the proposal for a person, and the log says why.
- **High risk is never delegated.** A mandate allows low or medium risk.
- **The rules are enforced twice.** The service checks them and explains refusals; the database checks them again with policies and triggers. A rule that lives only in the service is listed as such in `docs/threat-model.md`. Do not claim more than that document says.
- **Agents are principals of their own.** They act as actor kind `agent`, and the database writes the actor of every log event from the session.
- **The log is append-only**, hash linked, and verifiable without the database. It is tamper-evident, not tamper-proof. An event names what it is about by digest (`events.Digest`) rather than by content, so payloads stay off the permanent record and a later change to them still shows.
- **Done once.** Every request that changes something carries a name for itself: an idempotency key from the agent, the proposal's id for the decision, the delivery id to the receiver. A repeat returns the first answer and says it is a repeat.
- **Tenancy is row level security** per tenant on every table with tenant data, with the application role that never bypasses it. Sessions are global, by email.
- **Say what is true.** No claim of compliance, detection or security that the code and the threat model do not back.

## Technology

Go (library plus one binary), Postgres 18 with RLS, River for jobs in Postgres, sqlc, the official MCP Go SDK, Connect for the people's API (buf generates Go and TypeScript), React with Vite, TypeScript and Tailwind for the UI (embedded in the binary), OIDC through go-oidc, testcontainers for tests. No DI framework; `cmd/pgmandate/main.go` and `kernel.go` wire things by hand.

## Structure

```
kernel.go, execute.go   The kernel: actions (data and builtin), the MCP handler, the execution path.
                        At the root so an embedded host writes pgmandate.New.
sign/                   Signing and verification of deliveries. At the root: the one package receivers import.
modules/<module>/       The domain: tenants, agents, actions, mandates, proposals, deliveries, auth.
                        Each has internal/repo (sqlc) and, except auth, its own tests with a
                        test_base_test.go. auth is tested through api/.
pkg/                    Shared packages without domain logic: actor (request scope), db (pools, scoped
                        transactions, migrations), events (the hash chain), fault (coded errors),
                        injection, jobs (River), testkit (Postgres for tests)
schema/                 The migrations, one set for all modules (NNNN_name.up.sql / .down.sql)
api/                    The Connect handlers for people
mcp/                    The MCP server for agents
proto/ gen/             The Connect contract and its generated Go and TypeScript
web/                    The UI (React, Vite, Tailwind); embed.go serves the build
internal/testfix/       The shared fixture of the integration tests
examples/helpdesk/      An embedded host: a ticket table guarded by mandate.may_write_for and builtin actions
examples/webhook/       A receiver of deliveries that verifies signatures
cmd/pgmandate/          The service binary and the operator's command line, with the demo
deploy/                 Compose and Dockerfile
docs/                   design.md, threat-model.md, roadmap.md, decisions.md
```

- A module never imports another module's `internal/` (Go stops it). Modules reach each other through their root packages; the kernel wires what would otherwise be a cycle.
- `pkg/` holds no domain logic and never imports from `modules/`.
- The migrations are one set, not one per module: the triggers cross every table, and splitting them would hide that.

## Conventions

- **Everything is in English:** code, comments, docs, commit messages, SQL, identifiers.
- **Scope:** `actor` carries tenant, actor and run id. Everything that touches the database reads them from the context; `db.InTx` sets the session variables per transaction. A tenant id is never passed around as a loose parameter.
- **Errors:** `fault` codes. Return a code only when it is not Internal. `fault.FromDb` reads a refusal by a trigger or a policy as a coded error.
- **Transactions:** `db.InTx`. Keep them short. Never make outgoing calls inside one. Events are appended in the same transaction as the change; follow-up work is a River job inserted in the same transaction.
- **SQL:** when a table is added, the same migration decides `tenant_id` and its RLS policy, append-only triggers where rows must not change, `ON DELETE CASCADE` from tenants, and a test that another tenant cannot see the row. Queries inside triggers and functions filter by tenant themselves rather than relying on row level security. A migration that has reached `main` is never changed.
- **Names:** Go uses strict CamelCase (`TenantId`, `HttpClient`). Environment variables are `MANDATE_*`.
- **Logs:** `log/slog` with fields. Never log payloads, tokens or secrets.
- **Generation:** `make gen` (buf and sqlc). Generated code (`gen/`, `web/src/gen/`, `sqlc_*.go`) is checked in.
- **Frontend:** English text, the design tokens in `web/src/index.css`, no raw colour values. `make web` builds it into the binary; `make web-dev` gives hot reload on :5173 against the server on :8585. Checked with `tsc`, oxlint and `pnpm build`.
- **Secrets:** agent tokens and connector secrets are stored as hashes or, for connector secrets, as the bytes the server signs with; neither is ever logged or returned after the moment it is shown.

## Tests

- Integration tests first, against a real Postgres via `testkit`, with the same roles and migrations as production. Each module has a `test_base_test.go` with an `IntegrationSuite` (testify) and one suite per test file that embeds it; `internal/testfix` is the shared fixture. Use `s.Require()`.
- Fakes are written by hand. No mock framework.
- Every table with tenant data has a test that another tenant cannot read or write it. Every rule the database enforces has a test that bypasses the Go code and is refused by Postgres. What must hold when two things happen at once (the budget, one decision per proposal, the log) has a test that runs them at once.
- Run `go test -p 4 -timeout 600s ./...` (the parallelism bounds how many Postgres containers start at once). A single package: `go test -timeout 120s ./modules/proposals/`.
- A failing test is never dismissed as unrelated without an explanation of why the change cannot have caused it.
- Before handing over: `make lint`, `go vet`, and the tests of the packages you changed. Then review your own diff (in Claude Code, `/code-review`) and fix what you find, with a test per fix.

## Git

- Never work directly on `main`. Branches: `feat/…`, `fix/…`, `docs/…`, `chore/…`. Everything reaches `main` through a pull request.
- Conventional Commits with the package as scope: `feat(mandates): budget per agent`, `fix(proposals): lock the mandate while counting spend`.
- Security critical changes (schema, RLS, triggers, auth, MCP tools) are marked in the pull request.
- Decisions and changed requirements go into `docs/decisions.md` with a date, and into this file when they change the rules.
