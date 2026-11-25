# Design

How pgmandate is put together. The reasons behind the choices are in [decisions.md](decisions.md); what it does not protect against is in [threat-model.md](threat-model.md).

## The shape

One service and one Postgres, run locally, hosted or on-premise. Three doors:

- **Agents** connect over MCP with a token and propose.
- **Systems** receive what was decided through connectors: a signed webhook per action.
- **People** sign in to a web UI: the inbox, the mandates, the agents and actions, the people, and the evidence.

A host that shares the database can also embed the Go library and carry actions out with its own code inside the deciding transaction. `examples/helpdesk` shows that form; it is not the main one.

## Actions are data

An owner defines an action in the UI or on the command line:

| column | meaning |
|---|---|
| `key` | `tickets.create` |
| `description` | shown to agents and people |
| `risk` | low, medium or high |
| `cost_units` | charged to the mandate's budget when a mandate allows it |
| `may_execute` | whether a mandate may ever allow it without a person |
| `schema` | JSON schema of the payload, shown to agents, enforced on propose and on edit. Required for a webhook, so what reaches another system has a stated shape; `{"type":"object"}` states that anything goes |
| `connector` | `webhook` with a URL and a signing secret, or `builtin` for an embedded host's code |
| `department` | an optional label; nothing is scoped by it yet |

An embedded host can also register actions in code. A tenant's own definition of the same key takes precedence for everything except the code.

## The life of a proposal

1. The agent calls `propose` with an idempotency key. The payload is checked against the schema, the risk is set, the text is scanned, and the proposal is stored as waiting. If it could be allowed by a mandate, a job is inserted in the same transaction. The same key again returns this proposal and does nothing else.
2. The job asks, in one transaction with the mandate rows locked: is there a live mandate for this agent that covers the action, at this risk, with budget left, and is the action still as it was (its schema, cost and risk)? If so, the proposal becomes allowed, with the basis recorded. If not, it stays waiting and the reason goes to the log.
3. A person approves, edits and approves, or rejects what waits. An approval is held to the action as it is then: the payload to its schema, at its current risk if that is higher. The payload as the agent proposed it is kept beside an edit.
4. For a webhook action, the deciding transaction also records a delivery and inserts its job, on a queue of its own so a slow connector does not hold up decisions. The job posts the decision, signed, and records the answer as the result, or retries, or marks it failed. A person can send a failed delivery again.
5. The agent reads the decision, the delivery state and the result with `get_proposal`, which can wait for them.

The decision is final in the database whatever the connector does. "Allowed" and "delivered" are separate facts, and the agent is told both.

## Enforcement

Every transaction declares its scope: tenant, actor kind, actor id, the agent's task id, and, for an execution, the one proposal being executed. Row level security and triggers read that scope. The service checks the rules first and gives readable errors; the database checks them again. Which rules live where is listed in the threat model.

The log is one table per tenant, append-only, each event's hash covering the previous hash and a canonical encoding of the event. Its times come from the database's clock, read under the tenant's log lock, so several server instances write it in order. The database fixes the actor and requires each event to continue the chain. Events name what they are about by digest (the payload proposed, the payload decided, the body delivered, an action's URL and schema) rather than by content. Verification recomputes the chain, in the server or from an export (`verify-export`).

Hashes and digests are computed over a canonical JSON form, so they can be checked without the server: object keys sorted, no whitespace between tokens, numbers in plain decimal as Postgres's `jsonb` gives them back (`1.50e1` is `15.0`, never an exponent), strings as Go's `encoding/json` writes them (which escapes `<`, `>` and `&` as `\u003c`, `\u003e` and `\u0026`), and NUL characters, which `jsonb` refuses, replaced by U+FFFD. A digest is the lower-case hex SHA-256 of that form (`events.Digest`); a connector address's digest is the SHA-256 of the address as written.

A connector's secret is written by an owner and never read by the application, except by a transaction scoped as the system, through the `connector_of` function, which returns the action's current address and its secret together. A delivery is sent there; one recorded at an older address is moved first, and the move is logged.

## People and sign-in

A session is a random token; only its hash is stored. It reaches the server as a bearer header, or as an HttpOnly cookie set by the OpenID Connect callback. Dev login, with an email only, exists for local use and is refused unless the server listens on loopback and the request asks for localhost.

A person is a row per tenant, identified by email. The tenant a call acts in is chosen with the `Mandate-Tenant` header, and an interceptor checks that the signed-in email is a person there. Owners add and remove people. An operator creates tenants from the command line.

The API is Connect: Auth, Persons, Agents, Actions, Mandates, Proposals, Evidence.

## Deployment

One image, built in two stages so the web app is embedded in the binary. Configuration is by environment; see the README. `pgmandate migrate` runs as the owner, as a step of its own; `serve` runs with the application role only, and refuses the repository's default password when it listens beyond loopback. `deploy/compose.server.yaml` shows the split.

## Not built

Passkeys. Expiry and withdrawal of proposals. Replay of a task. Conditions on a mandate, such as an amount limit. Budgets per tenant across mandates. Encryption of connector secrets at rest. Pagination and streaming exports. Self-service sign-up.
