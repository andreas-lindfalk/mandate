# Design note: what 0.1.0 freezes

Status: accepted, 2026-11-26. Build step 1 (section 11) is built: the fresh schema, River's own schema, budgets as a count, expiry and the sweep. It covers roadmap items 5 (proposal expiry), 6 (dry run), 8 (delivery contract v2) and 9 (the agent contract). They are designed together because they share the proposal's states, the delivery's states and the contracts that 0.1.0 freezes. Everything is built on a fresh first migration: nothing before 0.1.0 is kept compatible ([decisions](decisions.md)).

This note sets out the states, the rules the database holds, the contracts, the jobs, the log events and the tests. When the code differs from this note, one of them is wrong.

## What changes, in short

- **Proposals:**
  - They get *expired*, and a dry run's *would allow* and *dismissed*.
  - They get a deadline for waiting and a window for delivery, both fixed when the proposal is made.
- **Budgets** count executions. "Units" and an action's cost go.
- **Deliveries:**
  - They are signed with Ed25519, over a body that names its kind, its audience and its tenant.
  - A receiver's answer is final only when it says so: *done*, *refused* or *unknown*. Everything else is retried.
  - At the end, a delivery is *done*, *refused*, *not sent* or *unknown*. A person settles *unknown*, and only once the work can no longer happen.
- **Agents** get one Connect contract, `mandate.agent.v1`, served over MCP and as plain JSON over HTTP.
- **Not in 0.1.0:** a result reported later (an *accepted* answer with a deadline for the result). It is purely additive, since a receiver that does not use it never sends it, so it can come in a minor version without a new contract. See section 9.

## 1. The proposal

### New columns

| Table | Column | Meaning |
|---|---|---|
| `actions` | `wait_limit interval NOT NULL DEFAULT '7 days'` | How long a proposal may wait for a decision. Between 1 minute and 30 days. |
| `actions` | `deliver_within interval NOT NULL DEFAULT '1 hour'` | How long after a decision the delivery may still be carried out. Between 2 minutes and 7 days. |
| `agents` | `mode text NOT NULL DEFAULT 'dry_run' CHECK (mode IN ('live', 'dry_run'))` | A new agent starts in dry run, so nothing it proposes is carried out until an owner makes it live. An owner may create it live, or switch it later. The switch is logged (`agent.mode_changed`). |
| `proposals` | `dry_run boolean NOT NULL` | Set by the insert trigger from the agent's mode. Never changes. |
| `proposals` | `expires_at timestamptz NOT NULL` | `created_at + wait_limit`, set by the insert trigger. |
| `proposals` | `deliver_within interval NOT NULL` | Copied from the action by the insert trigger, so a later change to the action changes nothing for this proposal. |

- **Actions that exist only as code.** A builtin action with no row in `actions` gets the defaults above. A builtin action runs inside the deciding transaction, so for it only `wait_limit` matters.
- **What goes.** `actions.cost_units` and `proposals.cost_units` go (section 2). `actions.connector_secret` goes (section 3).

### States

| Status | Live or dry run | Reached by | Means |
|---|---|---|---|
| `pending` | both | insert | Waits. For a dry run: what would have waited for a person. |
| `approved` | live | a person | Decided by a person, with an edit or without. |
| `rejected` | live | a person | Refused by a person. |
| `executed` | live | the system, under a mandate | Allowed by a mandate. |
| `withdrawn` | both | the proposing agent | Taken back before `expires_at`. |
| `expired` | both | the system | Nobody decided before `expires_at`. |
| `would_allow` | dry run | the system, under a mandate | A mandate would have allowed it. The basis is recorded. |
| `dismissed` | dry run | a person | Looked at and set aside. A person never approves a dry run. |

Every status except `pending` is final, as today.

### Rules the database holds

The triggers on `proposals` hold these. Each has a test that bypasses the Go code.

1. **The deadline cuts both ways.** At or after `expires_at`, the only change allowed is to `expired`, by the system. Before `expires_at`, `expired` is refused. The comparison uses the database's clock (`now()`, the transaction's start), so a decision whose transaction began before the deadline is in time. The threat model says so.
2. **A dry run** never becomes `approved`, `rejected` or `executed`. The system may set `would_allow`, with a basis that `basis_holds` accepts. A person may set `dismissed`.
3. **A live proposal** never becomes `would_allow` or `dismissed`.
4. **Nothing else for a dry run:**
   - `may_write_for` is false for it, so it can never open an embedded host's table.
   - The deliveries guard refuses a delivery for it.
5. **Spend counts only `executed`**, so dry runs are never in a budget.

### Expiry

- **The sweeper** expires proposals: see the jobs in section 4. In one transaction per proposal, as the system, it sets `expired` and writes `proposal.expired`.
- **Without a job queue.** An embedded host may run without one. There, reads report a proposal past `expires_at` as expired, and `Kernel.ExpireDue(ctx)` is exported for the host to call.
- **What the agent sees.** `get_proposal` answers "expired: nobody decided in time; nothing was done".
- **The inbox** has a filter for expired proposals.

### Dry run

- **The agent's side.** It proposes as usual and goes on with its own credential. Every answer carries `dryRun`, and the agent contract says: **act with your own credential only when the answer says `dryRun: true`**. A key reused after a switch from live to dry run returns the live proposal, which says `dryRun: false`. That proposal will be carried out, so the agent must not act on it itself.
- **Waiting proposals.** They show in the inbox marked as dry runs, with *Dismiss* as the only button, and they send no chat notice.
- **Budgets.** A dry run is evaluated against the live budget, but adds nothing to it. Many dry runs can therefore each show *would allow* against the same last place in a budget, so the dry-run figures are shown apart from the live ones.
- **Kept out of** the decisions export and the agreement figures. The log keeps them, with `dryRun: true` in each event, so the chain is whole.

## 2. Budgets: a count

- `mandates.budget_units` becomes `budget_count bigint NOT NULL CHECK (budget_count BETWEEN 1 AND 1000000000)`.
- **Spend** is the number of `executed` proposals under the mandate whose `decided_at` falls in the current UTC calendar month, by the database's clock, as today but counted.
- **An execution is allowed** when spend is below `budget_count`, counted under the mandate's row lock.
- **The basis** carries `budgets: [{kind: "count", limit, spent}]`, with `spent` counted before this execution. Money (phase 2) adds `{kind: "money", currency, limit, spent}` entries to the same list.
- **What "the action changed" means now,** without a cost:
  - The payload no longer fits the action's schema. The service holds this.
  - A webhook action's risk was raised. The service and the database both hold this.

  The scope's principle 3 is reworded in the same change.

## 3. Delivery contract v2

### The signing key

- **Where it comes from.** It is an Ed25519 key, read from `MANDATE_SIGNING_KEY` (the 32-byte seed in standard base64) or from `MANDATE_SIGNING_KEY_FILE` (the same, in a file). It is never stored in the database. An embedded host passes it in `Options`.
- **Local use.** When no key is given and the server listens only on loopback, it generates one into `.local/signing-key` with mode 0600, which git ignores, and says so in the log. Beyond loopback, a missing key stops the server from starting.
- **Key id.** The first 16 lower-case hex digits of the SHA-256 of the public key.
- **Rotation.** `MANDATE_SIGNING_KEYS_PREVIOUS`, a comma-separated list of standard base64 public keys, are published but never sign.
- **Publishing.** `GET /.well-known/pgmandate-keys` serves `{"keys": [{"kid", "alg": "Ed25519", "public"}]}` with every key that is current or previous.
  - A receiver either fetches the set, over https from an address it is configured with, or is configured with keys by hand.
  - A fetched set replaces what it cached; it never adds to it. Dropping a key from the set is how a key is revoked.

### The audience

- **What it is.** Each webhook action has an audience, `pgm:<tenantId>:<actionKey>:<16 random hex digits>`.
- **When it changes.** A new random part is drawn when the action is defined, defined again after removal, or moved to a new address. An audience therefore never repeats, and a receiver that still accepts an old one never gets a body for it again.
- **What it replaces.** The audience is shown to the owner where the secret is shown today. The secret goes.

### The request

```
POST <connector url>
Content-Type: application/json
Mandate-Delivery-Id: <uuid>
Mandate-Timestamp: <unix seconds by the database's clock, canonical decimal>
Mandate-Signature: v2 kid=<key id> sig=<standard base64 Ed25519 signature>
```

- **What is signed.** The exact bytes `pgmandate-v2.` + the timestamp header + `.` + the raw body.
- **One clock.** The timestamp is the database's clock, read in the transaction that opens the attempt. So `notAfter`, the timestamp and every margin below come from the same clock. A receiver whose clock is more than 5 minutes off refuses the signature at step 2.
- **`notAfter`** is `decided_at + deliver_within`, both from the database.
- **The body.** Within one address and audience, the body is the same on every attempt. A move gives a new audience and so a new body, and the log names its digest.

The body:

```json
{
  "version": 2,
  "kind": "delivery",
  "audience": "pgm:7d0c…:refunds.issue:9f2c…",
  "tenantId": "7d0c…",
  "deliveryId": "…",
  "correlationId": "…",
  "createdAt": "…",
  "notAfter": "…",
  "proposal": { "id", "action", "title", "payload", "risk", "agentId", "runId", "proposedAt" },
  "decision": {
    "status": "approved | executed",
    "decidedAt", "decidedBy", "note", "edited",
    "executionBasis": { "mandateId", "policyRef", "maxRisk", "condition",
                        "budgets": [ { "kind": "count", "limit": 40, "spent": 12 } ] }
  }
}
```

`kind` is `delivery`. `callback` is reserved for phase 2. The JSON Schema is published as `docs/delivery-v2.schema.json`, and `delivery-v1.schema.json` is removed.

### What a receiver does, in order

1. **Verifies** the signature over the raw header and the raw body, before parsing anything, with a key id it trusts.
2. **Checks the timestamp** is within 5 minutes of its clock.
3. **Parses, and checks** that `kind` is `delivery`, and that `audience` and `tenantId` are the ones it was configured with. It never takes them from the request. The delivery id it uses is the one in the body, which must equal the header's.
4. **Claims the delivery id atomically:**
   - If it finished this delivery before, it answers exactly as it did then.
   - If the delivery is still in progress, it answers 503, which is retried.
   - It keeps the ids it has seen until at least `notAfter` plus 5 minutes.
5. **Checks `notAfter`.** If its clock is past `notAfter` for a delivery it never started, it records and answers `{"outcome": "refused", "refusal": {"reason": "too_late"}}`, and does nothing. A repeat gets the same answer (step 4).
6. **Checks the facts** in the payload against its own records.
7. **Acts, and never commits after `notAfter`** by its own clock, even if the work is already under way. If it aborts work in progress and is sure nothing took effect, it answers `refused`. If it cannot be sure, it answers `unknown`.

### The answer

Only an explicit answer is final.

| Answer | Result |
|---|---|
| 2xx `{"outcome": "done", "result": {…}}` | `done`, with the result |
| any status `{"outcome": "refused", "refusal": {…}}` | `refused`. The receiver states it did nothing. A refusal is always this shape. |
| 2xx `{"outcome": "unknown", "reason": "…"}` | `unknown`. The receiver cannot tell, as when its own call timed out. |
| anything else: other 2xx bodies, 3xx (redirects are not followed), any other 4xx, 5xx, a timeout, a reset | Retried until the attempts run out or `notAfter` comes near. Then `unknown` if any attempt was sent, and `not_sent` if none was. |

Answers are JSON objects of at most 4 kB.

### How the worker knows what it may have sent

An attempt counts as possibly sent from the moment it starts, until it is proven otherwise. Three columns on `deliveries` hold this:
- `attempts_started int`
- `attempts_unsent int`: attempts proven never to have reached the receiver
- `attempt_open_at timestamptz`: set while an attempt is open

**Opening an attempt.** Before each call, in its own committed transaction, the worker opens an attempt: `attempts_started + 1`, and `attempt_open_at = now()`. The same transaction reads the database's clock for the signed timestamp. The guard refuses the opening when:
- `now() > not_after − 15 s`, so no attempt starts too late
- an attempt is already open and younger than the request timeout
- the delivery has moved since the job read it

An open attempt older than that was left by a crash. It stays counted as possibly sent, and a new one may open.

**Closing it.** After the call, the worker closes the attempt by clearing `attempt_open_at`, and records what it learned:
- **A failure before the request's headers were written** (`httptrace.WroteHeaders` never fired), such as DNS, a refused connection, a TLS handshake, a blocked or private address, or plain http, adds 1 to `attempts_unsent`. Only this proves an attempt unsent.
- **An explicit answer** is recorded as the answer table says.
- **Anything else** leaves the attempt counted as possibly sent.

**Possibly sent** means `attempts_started > attempts_unsent`, or an attempt is open. `not_sent` and a move both require that a delivery was not possibly sent.

**Timeouts and retries:**
- Each call's deadline is the smaller of 15 seconds and `not_after` minus now.
- River retries at most 8 times, with each retry capped at `not_after − 15 s`.
- A job that runs too late finds the guard refusing to open an attempt, and finishes the delivery instead: `unknown` if possibly sent, `not_sent` if not.

### Delivery states

| Status | Final | Means |
|---|---|---|
| `pending` | no | Not yet answered for certain. |
| `done` | yes | The receiver did it. |
| `refused` | yes | The receiver did nothing, and said so. |
| `not_sent` | no | No attempt was possibly sent. Nothing was done. A person may send it again before `not_after − 15 s`. |
| `unknown` | no | It may or may not have happened. A person settles it. |
| `settled_done`, `settled_not_done` | yes | A person settled an unknown delivery. |

The deliveries guard holds every transition, and refuses everything not listed:

| From | To | By | When |
|---|---|---|---|
| `pending` | `done`, `refused`, `unknown` | the system | on a receiver's answer |
| `pending` | `unknown` if possibly sent, otherwise `not_sent` | the system | no attempt open, and the attempts ran out or `not_after − 15 s` has passed |
| `not_sent` | `pending` | a person | before `not_after − 15 s`; same delivery id, the action's current address and audience |
| `unknown` | `settled_done`, `settled_not_done` | a person | after `not_after` + 15 s + 5 min, when the work can no longer happen |
| `pending`, not possibly sent, no attempt open | `pending` at a new address and audience | the owner who moves the action, in the same transaction | never once an attempt was possibly sent |

The first answer wins. A receiver's answer that arrives after a delivery has left `pending`, such as a slow 2xx after the worker gave up, is recorded as `delivery.conflict` for a person and never changes the status.

After `refused`, `settled_not_done`, or `not_sent` past `not_after − 15 s`, the agent may propose the same thing again under a new key, and the answer to the agent says so.

## 4. Jobs

- **One periodic River job, `mandate.sweep`,** runs every minute on River's leader only. It calls a SECURITY DEFINER function, `mandate.tenants_with_due_work()`, backed by partial indexes. The function returns only the ids of tenants that have one of:
  - a pending proposal past `expires_at`
  - a pending delivery past `not_after − 15 s` with no attempt open
  - an attempt open for longer than the request timeout
- **For each such tenant,** the sweep inserts a `mandate.sweep_tenant` job, unique per tenant while it waits or runs. The job scopes itself to the tenant as the system. It takes each row with `FOR UPDATE SKIP LOCKED`, so a delivery worker or a second sweep never acts on the same row at once, and it:
  - expires the proposals
  - closes attempts left open by a crash, which stay counted as possibly sent; such a delivery stays `pending` for its job to retry or finish
  - finishes deliveries past `not_after − 15 s` with no attempt open, as `unknown` if possibly sent and `not_sent` if not
  - writes the events
- **Without a queue,** an embedded host calls `Kernel.ExpireDue` and `Kernel.FinishDue`.

## 5. The agent contract: `mandate.agent.v1`

```proto
service AgentService {
  rpc ListActions(ListActionsRequest) returns (ListActionsResponse);
  rpc Propose(ProposeRequest) returns (ProposeResponse);
  rpc GetProposal(GetProposalRequest) returns (GetProposalResponse);
  rpc ListProposals(ListProposalsRequest) returns (ListProposalsResponse);
  rpc WithdrawProposal(WithdrawProposalRequest) returns (WithdrawProposalResponse);
}
```

- **Authentication.** A bearer agent token (`mnd_…`) and nothing else. A person's session, as a cookie or a header, is refused with `unauthenticated`. So is a revoked agent or token.
- **Payloads are JSON text.** `ProposeRequest.payload_json` is a string holding a JSON object; results and refusals are JSON text too. `google.protobuf.Struct` would turn every number into a double and break the exact comparisons conditions rely on. The MCP layer keeps reading the tool's raw arguments for the same reason. The idempotency hash is over the canonical JSON the log uses (`events.Digest`), as today.
- **One implementation.**
  - The service lives in `modules/agentapi`. The call limit, the 2-second wait in `Propose`, the wait of up to 50 seconds in `GetProposal`, and the guidance texts (`outcome`, `next`, `unmet`) move there from `mcp/`.
  - The MCP server becomes a thin layer: each tool calls the service in process, and returns its answer as text and as structured content.
  - Connect serves the same service as JSON over HTTP at `/mandate.agent.v1.AgentService/<Method>`.
- **What the proposal view adds:**
  - `dryRun`
  - `expiresAt`
  - the new statuses
  - for a delivery: its status, `notAfter`, the attempts, and the result or refusal
- **Errors:**
  - `invalid_argument`
  - `already_exists`, for a key reused with another request
  - `failed_precondition`, for withdrawing a decided or expired proposal
  - `permission_denied`
  - `not_found`
  - `resource_exhausted`, for rate limits, with the seconds to wait in the details
  - `unauthenticated`
  - `internal`

  MCP keeps its readable names for the same cases.
- **Isolation.** An agent reads only its own proposals. A test calls every method with a second agent's token and gets nothing of the first agent's.

## 6. What else changes

- **The people API:**
  - `DismissProposal` and `SettleDelivery`
  - `SetAgentMode`
  - the action's `wait_limit`, `deliver_within` and audience, in place of the secret
  - `budget_count` on mandates
  - resend only from `not_sent`
- **The web UI:**
  - **Inbox.** An expired filter, a dry-run badge with Dismiss only, and the new delivery states. *Settle* is enabled only once settling is allowed. *Send again* appears only for `not_sent`.
  - **Agents page.** The mode switch, and the timing fields and audience on actions.
  - **Mandates page.** The count.
  - **Evidence page.** Dry runs shown apart.
- **The command line:**
  - `action define` takes the timing fields and prints the audience.
  - `agent create` takes `-mode live`; without it an agent starts in dry run.
  - `receiver` takes a key set and an audience instead of secrets.
  - The demo shows expiry and a dry run.
- **Receivers.** `examples/webhook` and `examples/refunds` implement the receiver's order above, including the atomic claim and the `notAfter` rule. The `sign` package gets `Verify` for v2.
- **The decisions export** leaves out every status but `approved`, `rejected` and `executed`.

## 7. The fresh schema

Migrations 0001 to 0008 are folded into one new `0001_init`. Besides what this note adds, it:

- keeps the purge guard of 0008
- drops `cost_units` and `connector_secret`
- adds `connector_audience`
- replaces the delivery statuses, and adds `not_after`, `attempts_started`, `attempts_unsent` and `attempt_open_at` to `deliveries`
- moves River into a schema of its own (roadmap item 10)

A database from before is reset with `make reset`.

## 8. Log events

New events, each written in the transaction that makes the change:

- `proposal.expired`
- `proposal.would_allow`, with the basis
- `proposal.dismissed`
- `agent.mode_changed`
- `delivery.done`, with a digest of the result
- `delivery.refused`
- `delivery.not_sent`
- `delivery.unknown`
- `delivery.settled`, with who settled it and which way
- `delivery.conflict`
- `action.moved`, which now names the new audience

Events of a dry run carry `dryRun: true`.

## 9. Later, without a new contract

**A result reported later.** A receiver whose work outlasts a call answers `{"outcome": "accepted"}`, and the delivery waits for a result until `resultBy`. It adds:
- a status
- `resultBy` in the body
- a result token derived as an HMAC of the delivery id under a key outside the database, so it is the same on every attempt and never stored
- an endpoint found through a SECURITY DEFINER lookup, as agent tokens are

Receivers that never send `accepted` are unaffected, so it can come in a minor version.

## 10. Tests

Each rule gets a test. The rules the database holds get one that bypasses the Go code. A helper scoped as the owner moves timestamps back, so deadlines can be tested without waiting.

- **Proposals:**
  - at or after `expires_at`: approve, reject, execute and withdraw are refused, and expiry is allowed
  - before it: expiry is refused
  - a dry run is never approved or executed, gets no delivery, keeps `may_write_for` false, and is not counted in spend
  - a repeat key after a mode switch returns the first proposal, marked as what it was
- **Deliveries:**
  - every transition in the table, and a refusal for every one not in it
  - no attempt started after `not_after − 15 s`
  - a crash after an attempt opened ends as `unknown`, never `not_sent`, even if the retry then fails to dial
  - the sweep never finishes a delivery while an attempt is open
  - a move is refused while an attempt is open, or after one was possibly sent
  - a dial failure on every attempt, with none open, ends as `not_sent`
  - a proxy's 404 is never read as `refused`
  - a slow 2xx after the worker gave up is recorded as a conflict
- **Signing:**
  - a fixed test vector of key, timestamp and body
  - a body for one audience or tenant is refused under another
  - the kind `callback` is refused as a delivery
  - a key dropped from the fetched set no longer verifies
- **The agent contract:**
  - the same answer over MCP and over HTTP
  - a person's session refused
  - another agent's proposals invisible
  - a 30-digit number kept exactly on both paths
- **Receivers:** `examples/webhook` and `examples/refunds`, against v2, including a repeat in flight answered with 503.

## 11. Order of building

| Step | What | Size |
|---|---|---|
| 1 | The fresh schema, River's own schema, budgets as a count, the proposal's new columns and states, the sweeper, and expiry. The v1 body's basis carries the budgets list until step 2 replaces v1. | L |
| 2 | Delivery contract v2: the key, the audience, the answers, the attempt marker, the states, settling, and the receivers | L |
| 3 | The agent contract, with MCP moved onto it | M |
| 4 | The dry run | M |

Each step is a pull request with its tests. Steps 1 and 2 are the large ones. The changes in section 6 land with the step that needs them.

## 12. Settled questions

1. **Defaults.** A wait of 7 days and a delivery window of 1 hour, as defaults an owner overrides per action.
2. **Mode for new agents.** Dry run, so a new agent changes nothing until an owner makes it live. `agent create` and the UI let an owner create one live, and the demo and the quickstart do so, and say it.
3. **Waiting dry runs.** A person may only dismiss one. "Would approve" would look like a decision without being one, and the agreement figures come only from real decisions.
