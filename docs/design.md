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

1. The agent calls `propose` with an idempotency key and its account: a rationale (required), evidence (what it looked at: a source, a reference to find it again, what it showed) and provenance (the MCP client as it introduced itself, the model and its own version as it states them). The payload is checked against the schema, the risk is set, the text is scanned, including the account, and the proposal is stored as waiting, unless the agent is past its limit for the minute or the hour, which the database refuses. The account is not part of the request a key names: a retry in other words is the same request. If it could be allowed by a mandate, a job is inserted in the same transaction. The same key again returns this proposal and does nothing else.
2. The job asks, in one transaction with the mandate rows locked: is there a live mandate for this agent that covers the action, at this risk, whose condition holds for the payload, with budget left, and is the action still as it was (its schema, cost and risk)? If so, the proposal becomes allowed, with the basis recorded. If not, it stays waiting and the reason goes to the log.
3. A person approves, edits and approves, or rejects what waits, reading the agent's account beside the payload, and, when a condition stopped it, which comparison failed and what the payload had instead. When the tenant has a notifier, a Slack or Discord channel is told once that the proposal waits: the action, its risk, the agent's registered name, why it waits and a link, never the agent's text. An approval is held to the action as it is then: the payload to its schema, at its current risk if that is higher. The payload as the agent proposed it is kept beside an edit.
4. For a webhook action, the deciding transaction also records a delivery and inserts its job, on a queue of its own so a slow connector does not hold up decisions. The job posts the decision, signed, and records the answer as the result, or retries, or marks it failed. A person can send a failed delivery again.
5. The agent reads the decision, the delivery state and the result with `get_proposal`, which can wait for them.

The decision is final in the database whatever the connector does. "Allowed" and "delivered" are separate facts, and the agent is told both.

## Conditions

A mandate may narrow what it allows by the content of a proposal, with a condition in CEL: `payload.amount <= 500 && payload.currency in ["EUR", "SEK"]`. Only what the database can check as well is accepted: payload fields (`payload.a.b`, `payload["a-b"]`) and `action` compared with literal values by `==`, `!=`, `<`, `<=`, `>`, `>=` or `in [...]`, combined with `&&`, `||` and `!`, and `has(payload.field)`.

At the grant the service parses and type-checks the condition with cel-go, compiles it to a plan (a small JSON tree), and checks the fields against the schemas of the actions the mandate covers at that moment, defined or builtin (when one of them has no schema, the fields are not checked). The mandate row keeps the text and the plan, and neither changes. At execution the service evaluates the plan to choose a mandate, and the trigger evaluates the same plan with `mandate.condition_holds`. The two agree case for case:

- numbers compare exactly as decimals, strings by their bytes (collation "C"), booleans only by equality;
- a step into anything but an object, a missing field, or a value of another type than the literal is unknown, as SQL's NULL: `&&` and `||` follow SQL's three-valued logic, `!` keeps unknown, and only true allows.

The execution's basis records the condition, and the delivery carries it to the receiver.

## The agent's account

The rationale, the evidence and the provenance are kept on the proposal as the agent gave them, and no update can change them, a person's included; the person's view goes in the decision note. The proposal event in the log holds their digests, the provenance labels, which are short names in a narrow alphabet, and the digest of the action's schema the proposal was made against, so the record says which version of the agent, the model and the action were involved without holding prose. The account is evidence about the agent's reasoning, not about the world: a mandate never reads it, and a receiver that acts on facts checks them in its own records (see `examples/refunds`).

## Taking a proposal back

An agent may withdraw its own waiting proposal, with a reason the person reads, as when it proposes again with a correction. The proposal is then decided as withdrawn: no person can approve it, no mandate executes it, and nothing is delivered. The database lets only the proposing agent do it, only while it waits, and changes nothing else with it.

## A receiver's refusal

A receiver that refuses a delivery with a 4xx answers once and for all. When its answer is a JSON object of at most 4 kB, it is kept with the failed delivery, named in the log by digest, shown in the inbox and given to the agent as `refusal`, with advice not to send the same request again. An answer to an attempt that will be retried is not kept.

## Limits per agent

Each agent may make at most so many proposals a minute and an hour, 30 and 300 unless an owner sets others. A trigger counts the agent's recent proposals under a lock per agent, so two at once cannot both take the last place, and refuses the rest with SQLSTATE MD429, pgmandate's own code, which reaches the agent as `rate_limited`. A repeat of a request inserts nothing and is not counted, also when it waited while its first copy took the last place.

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

Passkeys. Expiry of proposals. Replay of a task. Conditions beyond comparisons with literal values, and a cost taken from the payload. Budgets per tenant across mandates. Encryption of connector secrets at rest. Pagination and streaming exports. Self-service sign-up.
