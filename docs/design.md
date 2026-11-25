# Design

How pgmandate is put together. What it is and is not is in [scope.md](scope.md). The reasons behind the choices are in [decisions.md](decisions.md); what it does not protect against is in [threat-model.md](threat-model.md).

## The shape

One service and one Postgres, on your own machine, in your cloud or on-premise. Three doors:

- **Agents** connect with a token and propose, through one contract, `mandate.agent.v1` (`modules/agentapi`), served over MCP and as JSON over HTTP.
- **Systems** receive what was decided through connectors: a signed webhook per action.
- **People** sign in to a web UI: the inbox, the mandates, the agents and actions, the people, and the evidence.

A host that shares the database can also embed the Go library and carry actions out with its own code inside the deciding transaction. `examples/helpdesk` shows that form; the service is the main one. The shapes are described in [scope.md](scope.md#where-it-fits).

## Actions are data

An owner defines an action in the UI or on the command line:

| column | meaning |
|---|---|
| `key` | `tickets.create` |
| `description` | shown to agents and people |
| `risk` | low, medium or high |
| `wait_limit` | how long a proposal for it waits for a decision before it expires, 1 minute to 30 days; 7 days by default |
| `deliver_within` | how long after a decision its delivery may still be carried out, 2 minutes to 7 days; 1 hour by default |
| `may_execute` | whether a mandate may ever allow it without a person |
| `schema` | JSON schema of the payload, shown to agents, enforced on propose and on edit. Required for a webhook, so what reaches another system has a stated shape; `{"type":"object"}` states that anything goes |
| `connector` | `webhook` with a URL and an audience the database draws, or `builtin` for an embedded host's code |
| `department` | an optional label; nothing is scoped by it yet |

An embedded host can also register actions in code. A tenant's own definition of the same key takes precedence for everything except the code.

## The life of a proposal

1. The agent calls `propose` with an idempotency key and its justification: a rationale (required), evidence (what it looked at: a source, a reference to find it again, what it showed) and provenance (the client, as the MCP client introduced itself or as the agent names it over HTTP, the model and its own version as it states them). The payload is checked against the schema, the risk is set, the text is scanned, including the justification, and the proposal is stored as waiting, unless the agent is past its limit for the minute or the hour, which the database refuses. The justification is not part of the request a key names: a retry in other words is the same request. If it could be allowed by a mandate, a job is inserted in the same transaction. The same key again returns this proposal and does nothing else.
2. The job asks, in one transaction with the mandate rows locked: is there a live mandate for this agent that covers the action, at this risk, whose condition holds for the payload, with room left in its count for the month, and does the payload still fit the action's schema, and is the action's risk no higher than it was? If so, the proposal becomes allowed, with the basis recorded. If not, it stays waiting and the reason goes to the log.
3. A person approves, edits and approves, or rejects what waits, reading the agent's justification beside the payload, and, when a condition stopped it, which comparison failed and what the payload had instead. When the tenant has a notifier, a Slack or Discord channel is told once that the proposal waits: the action, its risk, the agent's registered name, why it waits and a link, never the agent's text. An approval is held to the action as it is then: the payload to its schema, at its current risk if that is higher. The payload as the agent proposed it is kept beside an edit.
4. For a webhook action, the deciding transaction also records a delivery, with its deadline (`notAfter`, the decision plus the action's delivery window), and inserts its job, on a queue of its own so a slow connector does not hold up decisions. The job opens an attempt in a committed transaction, posts the decision signed with the server's key, and records the receiver's explicit answer: *done* with its result, or *refused*, or *unknown*. Anything else is retried, never later than the deadline allows. When it gives up, the delivery is *unknown* if an attempt may have reached the receiver and *not sent* if none did. A person may send a delivery that was never sent again before the deadline, and settles an unknown one as done or not done once the receiver can no longer act. Delivery contract v2 is set out in [design-0.1.md](design-0.1.md) and the README.
5. The agent reads the decision, the delivery state and the result with `get_proposal`, which can wait for them.

The decision is final in the database whatever the connector does. "Allowed" and "carried out" are separate facts, and the agent is told both.

## Conditions

A mandate may narrow what it allows by the content of a proposal, with a condition in CEL: `payload.amount <= 500 && payload.currency in ["EUR", "SEK"]`. Only what the database can check as well is accepted: payload fields (`payload.a.b`, `payload["a-b"]`) and `action` compared with literal values by `==`, `!=`, `<`, `<=`, `>`, `>=` or `in [...]`, combined with `&&`, `||` and `!`, and `has(payload.field)`.

At the grant the service parses and type-checks the condition with cel-go, compiles it to a plan (a small JSON tree), and checks the fields against the schemas of the actions the mandate covers at that moment, defined or builtin (when one of them has no schema, the fields are not checked). The mandate row keeps the text and the plan, and neither changes. At execution the service evaluates the plan to choose a mandate, and the trigger evaluates the same plan with `mandate.condition_holds`. The two agree case for case:

- numbers compare exactly as decimals, strings by their bytes (collation "C"), booleans only by equality;
- a step into anything but an object, a missing field, or a value of another type than the literal is unknown, as SQL's NULL: `&&` and `||` follow SQL's three-valued logic, `!` keeps unknown, and only true allows.

The execution's basis records the condition, and the delivery carries it to the receiver.

## The agent's justification

The rationale, the evidence and the provenance are kept on the proposal as the agent gave them, and no update can change them, a person's included; the person's view goes in the decision note. The proposal event in the log holds their digests, the provenance labels, which are short names in a narrow alphabet, and the digest of the action's schema the proposal was made against, so the record says which version of the agent, the model and the action were involved without holding prose. The justification is evidence about the agent's reasoning, not about the world: a mandate never reads it, and a receiver that acts on facts checks them in its own records (see `examples/refunds`).

## Taking a proposal back

An agent may withdraw its own waiting proposal, with a reason the person reads, as when it proposes again with a correction. The proposal is then decided as withdrawn: no person can approve it, no mandate executes it, and nothing is delivered. The database lets only the proposing agent do it, only while it waits, and changes nothing else with it.

## A receiver's refusal

A receiver that refuses a delivery says so explicitly, `{"outcome":"refused","refusal":{…}}`, at any status, and that is final; a bare 4xx is not, since a proxy's 404 says nothing about what the receiver did. The refusal, a JSON object of at most 4 kB, is kept with the delivery, named in the log by digest, shown in the inbox and given to the agent as `refusal`, with advice not to send the same request again. An answer to an attempt that will be retried is not kept.

## Expiry

A proposal waits until a person decides it, its agent withdraws it, or its deadline passes. The deadline (`expires_at`) and the delivery window (`deliver_within`) are fixed by the insert trigger from the action, or from the defaults for an action that exists only as code, so a later change to the action changes nothing for a proposal already made.

From the deadline on, the update trigger allows one change only: the system setting the status to `expired`, with nothing else changed. An approval, a rejection, a withdrawal and an execution are all refused. Before the deadline, nothing expires a proposal. The comparison uses the transaction's start (`now()`), so a decision whose transaction began before the deadline is in time. The service checks the same thing first, to give a readable error.

A periodic River job, `mandate.sweep`, runs every minute on River's leader. It asks `mandate.tenants_with_due_work()`, a function that returns tenant ids only, which tenants have proposals past their deadline, and queues one `mandate.sweep_tenant` job per tenant. That job, scoped to the tenant as the system, expires the due proposals one transaction each, taking each row with `FOR UPDATE SKIP LOCKED` so a decision under way is never waited on, and writes `proposal.expired`. A host that runs the kernel without a queue calls `Kernel.ExpireDue` itself. An agent that asks about a proposal past its deadline before the sweep has come is told it has expired.

## Limits per agent

Each agent may make at most so many proposals a minute and an hour, 30 and 300 unless an owner sets others. A trigger counts the agent's recent proposals under a lock per agent, so two at once cannot both take the last place, and refuses the rest with SQLSTATE MD429, pgmandate's own code, which reaches the agent as `rate_limited`. A repeat of a request inserts nothing and is not counted, also when it waited while its first copy took the last place.

## Enforcement

Every transaction declares its scope: tenant, actor kind, actor id, the agent's run id, and, for an execution, the one proposal being executed. Row level security and triggers read that scope. The service checks the rules first and gives readable errors; the database checks them again. Which rules live where is listed in the threat model.

The log is one table per tenant, append-only, each event's hash covering the previous hash and a canonical encoding of the event. Its times come from the database's clock, read under the tenant's log lock, so several server instances write it in order. The database fixes the actor and requires each event to continue the chain. Events name what they are about by digest (the payload proposed, the payload decided, the body delivered, an action's URL and schema) rather than by content. Verification recomputes the chain, in the server or from an export (`verify-export`).

Hashes and digests are computed over a canonical JSON form, so they can be checked without the server: object keys sorted, no whitespace between tokens, numbers in plain decimal as Postgres's `jsonb` gives them back (`1.50e1` is `15.0`, never an exponent), strings as Go's `encoding/json` writes them (which escapes `<`, `>` and `&` as `\u003c`, `\u003e` and `\u0026`), and NUL characters, which `jsonb` refuses, replaced by U+FFFD. A digest is the lower-case hex SHA-256 of that form (`events.Digest`); a connector address's digest is the SHA-256 of the address as written.

Deliveries are signed with an Ed25519 key from the environment, never stored in the database, and published at `/.well-known/pgmandate-keys`. Each webhook action has an audience, `pgm:<tenant>:<key>:<random>`, drawn by the database when the action is defined, defined again after removal, or moved, and named in the signed body, so a receiver accepts only deliveries for its own action and tenant. A delivery is sent to the action's address and audience as recorded; when an owner moves the action, deliveries never possibly sent move with it in the same transaction, and the move is logged.

## People and sign-in

A session is a random token; only its hash is stored. It reaches the server as a bearer header, or as an HttpOnly cookie set by the OpenID Connect callback. Dev login, with an email only, exists for local use and is refused unless the server listens on loopback and the request asks for localhost.

A person is a row per tenant, identified by email. The tenant a call acts in is chosen with the `Mandate-Tenant` header, and an interceptor checks that the signed-in email is a person there. Owners add and remove people. An operator creates tenants from the command line.

The API is Connect: Auth, Persons, Agents, Actions, Mandates, Proposals, Notifiers, Evidence. The web UI uses only this API, except for sign-in, which is an OpenID Connect redirect, so another UI or an admin tool can do everything else it does.

## Deployment

One image, built in two stages so the web app is embedded in the binary. Configuration is by environment; see the README. `pgmandate migrate` runs as the owner, as a step of its own; `serve` runs with the application role only, and refuses the repository's default password when it listens beyond loopback. `deploy/compose.server.yaml` shows the split.

River's tables live in a schema of their own, `mandate_river`, which `migrate` creates. The application role is given rights there and on nothing of River's in `public`, so a host that runs River itself, in `public`, never shares a queue with pgmandate.

## Not built

Passkeys. Replay of a run. Conditions beyond comparisons with literal values, and budgets in money. Budgets per tenant across mandates. Pagination and streaming exports. Self-service sign-up.
