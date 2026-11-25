# pgmandate

**Agents propose. A person, or a person's standing permission, decides. Postgres is the last gate.**

pgmandate is a reference implementation of write authority for AI agents. The idea: an agent should hold no credentials to the systems it changes. It can only *propose* an action. Then one of two things happens:

- a **person** approves, edits or rejects the proposal in an inbox, or
- a **mandate** that a person granted earlier allows it at once, and the proposal records exactly which mandate, which policy, which risk limit and how much of the budget allowed it.

A mandate is data, not prompt text: *this agent's proposals for these actions may be carried out without a person, up to this risk, within this budget per month, until this date*, with a reference to the policy it rests on. (The reference is recorded with every decision it allows; the document itself is not stored or checked.) The decided proposal is then delivered, signed, to the system that carries it out, and what that system answers comes back to the agent.

The rules are enforced twice: in the service, and again in Postgres with row level security and triggers, so a bug in the service cannot record or allow what the rules refuse. Every proposal, decision, delivery and refusal is a row in a hash linked log that can be checked without the server.

Think of pull requests for agent actions: a proposal is the pull request, a mandate is the branch rule that allows auto-merge, the log is the history.

**Why not give the agent an API key and an approval step in its framework?** That works for one agent in one framework, and the agent still holds a key it could use without asking. Gateways fix the key, and some let an approval cover the next minutes or exempt a trusted agent. A mandate is a different kind of permission: it carries a budget, a risk limit, a policy reference and an end date, it is a row a named person granted, and Postgres checks it again on every decision it allows.

> **Status: early, a working demonstration.** It runs, it is tested against a real Postgres, and the demo below shows all of it. It has no production users and no external security review, connector secrets are stored unencrypted, and every system it protects needs a receiver written for it. Read [the threat model](docs/threat-model.md) before relying on it.

## Three doors

```
agents ──MCP──▶ propose ──▶ the record (Postgres)
                               │  a person decides, or a mandate allows
                               ▼
systems ◀──signed webhook── delivery ──result──▶ back to the agent

people ◀──web UI / API──▶ inbox, mandates, agents and actions, people, evidence
```

- **Agents** connect over MCP with a token. Four tools: `list_actions`, `propose`, `get_proposal`, `list_proposals`. Any MCP client works: Claude Code, an SDK, a cron job.
- **Systems** receive decided proposals at the webhook URL of each action, signed with the action's secret. Deliveries are retried, and a person can send a failed one again.
- **People** sign in (OpenID Connect, or dev login on localhost) and run the tenant: the inbox, the mandates with their budget, the agents and their tokens, the actions and their connectors, the people, and the evidence with exports.

One binary and one Postgres, run locally, hosted or on-premise. The interface and the command line say it shorter, "agents propose, the database decides": the database is the last gate, and the people and their mandates are what it checks against. How the parts fit is in [docs/design.md](docs/design.md).

## What this requires, and what it is not for

- **The agent must have no other way to act.** pgmandate means something only if the credentials to the target system live with the connector, not with the agent. Taking them away is the real work, and it is an organisational change, not an install.
- **Keep the agent away from the admin path.** Locally, dev login and the command line's `-as EMAIL` act as any person, and the default database password is in this repository. An agent with a shell on the same machine could approve its own proposals. Run the agent as another user or in a container, and run pgmandate for real (below) before an agent depends on it.
- **It governs changes, not reads.** An agent reads what it needs through its own tools. pgmandate does not see or limit that.
- **It suits consequential actions**: a refund, a message that leaves the company, a change to a record. It does not suit a tight loop of small tool calls.
- **It is not a compliance tool.** If you run a high-risk system under the EU AI Act, or an ISO/IEC 42001 management system, the log is the kind of record that supports human-oversight and logging controls: who approved, under which authority, and what was refused. Whether those duties apply to you, and whether this is enough, is for you and your auditor.

## Five minutes

```sh
make up      # Postgres on 127.0.0.1:5433                       needs Docker
make demo    # the scripted demo, in the terminal               needs Go 1.27
make run     # the UI, the API and MCP on http://127.0.0.1:8585  also needs Node 22 and pnpm 10 (corepack enable)
```

`make demo` sets up a tenant, an owner, an agent, three webhook actions and a mandate, with a small receiver standing in for a helpdesk. The agent then proposes over MCP, exactly as any MCP client would. The demo prints eleven steps:

1. The setup: tenant, owner, agent, three actions, one mandate for `tickets.*` up to low risk with a budget of three units.
2. The agent asks what it may do.
3. A ticket within the mandate is allowed at once, delivered with a signature the helpdesk verifies, and the helpdesk's answer comes back to the agent.
4. Closing a ticket is medium risk, above the mandate: it waits for a person, and the reason is recorded.
5. A ticket whose text matches a known attempt to steer a model is flagged, high risk, and waits.
6. A refund is high risk, and high risk is never delegated: it waits.
7. Two more tickets use up the budget; the next one waits. One delivery is made to fail once, to show the retry.
8. The owner approves one waiting proposal and rejects another.
9. Five statements that no rule allows are sent straight to Postgres, as the system, the owner and the agent. Each is refused, with the database's own message.
10. The log verifies.
11. The first decided proposal is shown as it is exported.

It ends with the commands for what to do next. In short:

- `make run`, open http://127.0.0.1:8585, sign in as `alice@acme.example`, and you are in the inbox the demo left behind.
- The demo's receiver stops with the demo. Start the one it prints (`go run ./cmd/pgmandate receiver ...`) in another terminal, so that what you approve has somewhere to land. Without it a delivery retries and then shows as not delivered, and you can send it again later.
- The token the demo prints connects your own agent (next section). Its budget is spent, so grant a new mandate in the UI to see proposals go through by themselves.

Each `make demo` adds another tenant with the same name; `make reset` starts clean.

## Connecting an agent

An agent needs the MCP address and its token, which is shown once when it is issued (in the UI under *Agents & actions*, or by `pgmandate agent create`). In Claude Code:

```sh
claude mcp add --transport http pgmandate http://127.0.0.1:8585/mcp --header "Authorization: Bearer mnd_..."
```

Any other MCP client takes the same in its configuration:

```json
{ "mcpServers": { "pgmandate": { "type": "http", "url": "http://127.0.0.1:8585/mcp",
    "headers": { "Authorization": "Bearer mnd_..." } } } }
```

The server tells the agent how to work with it. In short: call `list_actions`, then `propose` with an `idempotency_key` made from its run and step; the answer says whether it was allowed, delivered, or waits for a person and why. Each answer is both text for a model and structured content for code, and an error carries a code (`invalid_argument`, `conflict`, `permission_denied`, `not_found`, `rate_limited`).

For whoever runs the agent:

- **Give each run a run id**, in the prompt or the task. Keys are made from it, and a run that reuses one gets the earlier run's answers. An agent given none takes `suggestedRunId` from `list_actions`.
- **Allow tool calls at least five seconds.** `propose` waits up to two seconds for a mandate's decision, and `get_proposal` can wait longer when asked; some clients give up sooner by default.
- An agent's calls are limited to five a second, with bursts of thirty.

To try an action without an agent, propose from the command line, over MCP like any client:

```sh
go run ./cmd/pgmandate propose -token mnd_... -action tickets.create -title "Printer jams" -payload '{"subject":"Printer jams"}'
```

It prints the answer and the run and key it used; the same `-run` and `-key` again is a repeat.

## Your own agent, your own system

From the repository, as the first owner:

```sh
make up && make migrate                    # Postgres, and the schema and the app role in it
go build -o pgmandate ./cmd/pgmandate      # the command line, as ./pgmandate in any terminal here

./pgmandate tenant create -name Acme -owner alice@acme.example -owner-name Alice
# tenant 7d0c…-…  (Acme)
# owner  …
T=<the tenant id it printed>

# What the agent may ask for, and where it goes. The secret is printed once.
./pgmandate action define -tenant $T -as alice@acme.example -key tickets.create -risk low -cost 1 -may-execute \
  -url http://127.0.0.1:8586/ -schema '{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}'

./pgmandate receiver -secrets mws_...      # in another terminal, in this directory: your system, for now

./pgmandate agent create -tenant $T -as alice@acme.example -name helpdesk-bot      # prints the agent id and its token
./pgmandate grant -tenant $T -as alice@acme.example -agent <agent id> -actions 'tickets.*' -max-risk low -budget 20 -policy "AI policy v1"

make run                                   # then connect the agent as above
```

The agent's `tickets.create` proposals are now allowed and delivered, twenty units a month; everything else waits in the inbox at http://127.0.0.1:8585. Without `-may-execute` a person always decides, and without `-agent` a mandate covers every agent of the tenant. `action define` sets the whole definition each time: a flag left out goes back to its default. Replace the receiver with your own (below).

## Running it

One image, one Postgres. A one-off `migrate` container holds the database owner's credentials; the server connects only as the application role:

```sh
export MANDATE_APP_PASSWORD=$(openssl rand -hex 16) MANDATE_OWNER_PASSWORD=$(openssl rand -hex 16)
export MANDATE_OIDC_ISSUER=... MANDATE_OIDC_CLIENT_ID=... MANDATE_OIDC_CLIENT_SECRET=...
docker compose -f deploy/compose.yaml -f deploy/compose.server.yaml up -d --build
```

Keep both passwords, in an `.env` file next to the compose files or a secret store: the owner password is fixed when the database volume is first created, and each start of `migrate` sets the app role's password again. This stack has its own database volume, apart from the one `make up` uses. Nobody can sign in until the provider is set, since dev login answers only on loopback. A receiver on the same machine is `http://host.docker.internal:<port>` from inside the container, a private address that needs `MANDATE_ALLOW_PRIVATE_CONNECTORS=true`; on Linux the receiver must listen on `0.0.0.0` (`-addr 0.0.0.0:8586`) to be reached that way. Then create a tenant with its first owner:

```sh
docker compose -f deploy/compose.yaml -f deploy/compose.server.yaml run --rm \
  migrate tenant create -name "Acme" -owner alice@acme.example -owner-name Alice
```

The administrative commands act on the database as the application role, at the address and with the app password worked out from the owner connection, so they run where that connection is set: in the `migrate` service here.

From there the owner works in the UI. Locally the same command is `go run ./cmd/pgmandate tenant create ...`, and `go run ./cmd/pgmandate help` lists the rest.

| variable | meaning |
|---|---|
| `MANDATE_DATABASE_URL` | the owner connection, for `migrate`; defaults to the local Postgres |
| `MANDATE_APP_PASSWORD` | the password `migrate` sets for the `mandate_app` role. The default, `app`, is refused when the server listens beyond loopback |
| `MANDATE_OWNER_PASSWORD` | compose only: the database owner's password when its volume is first created, default `owner` |
| `MANDATE_AGENT_TOKEN` | the token `pgmandate propose` uses when `-token` is not given |
| `MANDATE_APP_DATABASE_URL` | connect as the app role directly. With it, and without the owner connection, `serve` does not migrate and never holds the owner's credentials. This is how to run it for real: `pgmandate migrate` as a separate step |
| `MANDATE_HTTP_ADDR` | where to listen, default `127.0.0.1:8585` |
| `MANDATE_PUBLIC_URL` | how the server is reached, for cookies and the sign-in redirect |
| `MANDATE_OIDC_ISSUER`, `MANDATE_OIDC_CLIENT_ID`, `MANDATE_OIDC_CLIENT_SECRET` | the sign-in provider. It must serve one organisation: a person is whoever it vouches for with a verified email address |
| `MANDATE_OIDC_REDIRECT_URL` | default `<public url>/auth/oidc/callback` |
| `MANDATE_DEV_LOGIN` | `true` lets anyone sign in with an email. Refused unless the server listens on loopback, and then it answers only to requests for localhost |
| `MANDATE_SESSION_TTL` | default `168h` |
| `MANDATE_ALLOW_PRIVATE_CONNECTORS` | `true` lets webhook connectors live on local and private addresses, for development and for an on-premise install. Off, a delivery to such an address is refused and marked failed |
| `MANDATE_RECEIVER_ADDR` | where `pgmandate receiver` and the demo's receiver listen, default `127.0.0.1:8586` |

## Writing a receiver

A decided proposal is posted to its action's URL:

```
POST <url>
Content-Type: application/json
User-Agent: pgmandate
Mandate-Delivery-Id: <uuid>
Mandate-Timestamp: <unix seconds>
Mandate-Signature: v1=<hex hmac-sha256(secret, timestamp + "." + body)>

{ "version": 1, "deliveryId", "correlationId", "tenantId", "createdAt",
  "proposal": { "id", "action", "title", "payload", "risk", "agentId", "runId"?, "proposedAt" },
  "decision": { "status": "approved" | "executed", "decidedAt", "edited", "decidedBy"?, "note"?, "executionBasis"? } }
```

The body is described field by field in [docs/delivery-v1.schema.json](docs/delivery-v1.schema.json), a JSON Schema to validate against or generate types from in any language. A test holds the server to it.

- **Verify the signature** before anything else. It is HMAC-SHA256 with the secret's UTF-8 bytes, exactly as shown (`mws_...`), as the key, over the timestamp header's digits, a `.`, and the raw body bytes as received; the hex is lower case. Read the raw body, not a parsed one: `express.raw({ type: "application/json" })` in Express, `request.get_data()` in Flask. Compare in constant time, and refuse a timestamp more than five minutes off. In Go, `sign.Verify` does both. To check your own code: the secret `mws_example_secret_0123456789abcdef`, the timestamp `1767225600` and the body `{"version":1,"deliveryId":"00000000-0000-0000-0000-000000000001"}` give `v1=7d1b37c2f39b25a83f48808b5326479a4ad2265ee5b7fc034352bb047355f0d9`.
- **Check it is for you.** Each action has its own secret. A receiver that serves several actions keeps which tenant and action each secret belongs to, and refuses a body whose `tenantId` and `proposal.action` do not match the secret that signed it. Act only on `decision.status` `approved` or `executed`.
- **Check the version.** A field can be added within a version, so ignore fields you do not know. A removed field or a changed meaning is a new version: refuse a version you do not know.
- **Log the correlation id.** It is the run id the agent gave when it proposed, otherwise the proposal's id, so the agent's logs, pgmandate's log and yours can be joined on one value. A run id is the agent's own word: use it to group, and use `proposal.id` and `agentId` to identify.
- **Times** are RFC 3339 in UTC. `createdAt`, `proposedAt` and `decidedAt` are the same on every attempt; the time of an attempt is the `Mandate-Timestamp` header.
- **Do it once.** Delivery is at least once: the same delivery can arrive again after a timeout. Remember the `deliveryId`, somewhere that survives a restart, and answer the same way.
- **Answer 2xx** to accept. If the answer is a JSON object of at most 4 kB, it is kept as the proposal's result and shown to the agent, so return what it needs, such as the id of what you created.
- **Answer within 15 seconds**, or the attempt counts as failed. Do slow work after answering, keyed by the delivery id.
- **Other answers.** A 401 is retried, as are 408, 429, 5xx and no answer at all; any other 4xx means the request is wrong and is not retried. Retries go up to eight attempts, waiting about attempt⁴ seconds after each (1 s, 16 s, 81 s, and so on, with some jitter: about 80 minutes in all). After that the delivery is marked failed, and a person can send it again. Answer 401 for a signature you cannot verify, and 403 for a delivery you will never accept.
- **Redirects are not followed.** A 3xx counts as a failure and is retried until it fails. Give the exact URL, with `https` and the path your server expects.
- **Plain http only on private addresses.** Unless the operator allows local and private connectors, a connector must be `https`.
- **Rotating the secret.** Each attempt is signed with the action's secret as it is at that moment, read together with its address. After a rotation a receiver that does not yet have the new secret answers 401, and the delivery is tried again until it does, for up to the 80 minutes above. When an action's URL changes it gets a new secret, and the deliveries still waiting move to the new URL with it; apart from an attempt already in flight at that moment, the receiver at the old address gets nothing more.
- **No ordering.** Two deliveries from the same agent may arrive in either order.
- **The payload fits the action's schema** as the schema was when the decision was made. If the schema changed after the agent proposed, an approval needs an edit that fits and a mandate does not allow it.
- Fields marked `?` are left out when empty: `runId` when the agent gave none, `decidedBy` for an execution, `executionBasis` for an approval.

`examples/webhook` is a reference handler to mount in your own server, and `pgmandate receiver` runs it locally.

## Doing it once

Agents repeat themselves: a tool call times out and is sent again, or a model is not sure a step went through. Each hop has one name for the work, so a repeat is recognised and nothing is done twice.

| Hop | Name | What a repeat gets |
|---|---|---|
| Agent to pgmandate | `idempotency_key`, required on every `propose`, unique per agent | The first proposal, as it stands now: waiting, decided, delivered, with its result. `created` is false |
| Proposal to decision | The proposal's id | A proposal is decided once. A second approval, rejection or execution is refused |
| pgmandate to receiver | `deliveryId`, one per proposal, the same on every attempt | The receiver's first answer, if the receiver remembers the id |

- **No key, no proposal.** The table refuses a proposal without one, not only the service. A key is 1 to 200 printable ASCII characters without spaces, such as `run-9:step-3`.
- **The key names one request.** The same key with another action or payload is refused, so a reused key cannot quietly return an old answer to a new question. What was asked for is kept as a hash, so the repeat is still recognised after a person has edited the proposal, and after the action has changed or been removed.
- **Keys are the agent's to choose, and used once per agent, ever.** Its host should give each run a run id to make them from; `list_actions` offers `suggestedRunId` to an agent given none. Make them from the work, such as the run id and the step, and reuse them only for a retry of the same request. A key that comes back next week, such as `weekly-report`, returns last week's proposal: the answer then says `repeat: true` and that nothing new was done. An agent that makes up a new key for a retry has made a new request, and pgmandate cannot tell; the budget and the person are what is left then.
- **The receiver must remember delivery ids durably.** Use the id as the idempotency key of whatever you call next, or store it in the same transaction as the work. `examples/webhook` keeps them in memory, which is enough for a demonstration and not for a restart.

## The rules, in one place

- A proposal is allowed without a person only if **all** of these hold: the action is defined as one that can be delegated, a person granted a mandate that is live now and covers the agent and the action, the proposal's risk is within the mandate, nothing was flagged, and the month's budget has room for the cost. The agent and the inbox are told which one failed.
- A mandate allows low or medium risk. High is never delegated. A person's edit is assessed again and can only raise the risk.
- The action as it is when the decision is made counts: its schema, its cost and its risk. A proposal made before the action changed waits for a person, who approves it at the action's current risk or edits it to fit.
- A delivery is recorded in the transaction that makes the decision, and only to the action's own connector. The log names the payload that was proposed, the one decided on and the body that was sent, as digests.
- Only a person decides. Only an owner grants mandates, defines actions and adds or removes people. Only the system allows under a mandate, and only inside the transaction that names the proposal, with a basis the database checks against the mandate: coverage, risk limit, policy reference, budget and this month's spend.
- The log is append-only and hash linked per tenant, and each event carries the actor of its session. It is **tamper-evident, not tamper-proof**: an edit, a removal or a forged actor by the application is refused or detectable, and an export verifies offline, but someone who owns the database could rebuild it. Keep the latest hash somewhere the database owner cannot reach.
- The check for steering text is a **tripwire**, a few known English phrasings. It is not a control and will miss most real attempts. The controls are the action's schema and risk, the mandate, and the person.

What the database enforces, what only the service enforces, and what neither does, is spelled out in [docs/threat-model.md](docs/threat-model.md). The triggers in [schema/0001_init.up.sql](schema/0001_init.up.sql) are the authoritative text.

## Glossary

| Term | Meaning | Elsewhere called |
|---|---|---|
| Action | Something an agent may ask to have done, such as `tickets.create`, with a payload schema (required for a webhook), a risk and a cost | Tool call, operation |
| Proposal | One request by an agent to have an action carried out | Approval request, human in the loop |
| Mandate | A person's standing permission for an agent: which actions, up to which risk, within which budget, under which policy, until when | Policy, delegation, standing authorisation |
| Execution basis | What allowed a proposal without a person: the mandate, its policy reference, its risk limit and the budget at that moment | Authorisation record |
| Connector | Where a decided proposal is sent to be carried out: a signed webhook | Integration, webhook target |
| Delivery | One decided proposal on its way to a connector, with its attempts and the result | Webhook delivery |
| Log, or chain | The append-only, hash linked record of everything that happened in a tenant | Audit log, audit trail |
| Tenant, owner, reviewer | The organisation; a person who grants, defines and decides; a person who decides | Organisation, admin, approver |

The word is used elsewhere too: Google's Agent Payments Protocol calls its signed payment authorisations mandates, and an IETF draft describes agent mandates across domains. pgmandate is unrelated to both: its mandates cover any action, and they are rows in Postgres, not signed credentials.

## How it compares

Keeping credentials away from agents and asking a person before risky calls is, by late 2026, a common pattern. What others ship, as far as their own documentation says in November 2026:

- **MCP gateways.** TrueFoundry and Permit.io's MCP gateway ask a person before a tool call, and can let an approval cover a time window or exempt a trusted agent. Docker's MCP gateway can require an in-session confirmation through its sandbox policies. Kong's MCP proxy and Enkrypt's gateway apply allow and deny rules and guardrails.
- **Approval layers of the same kind.** SidClaw wraps an MCP server with policy, approval and a hash-chained audit. Countersign ("the model proposes, deterministic code disposes") is a reference implementation with a hash-chained audit and two approvers for high risk. OpenLeash lets owners set policies that agents ask under, and lets agents propose new policies.
- **Credential brokers**, such as Infisical Agent Vault (in preview) and Auth0's Token Vault with asynchronous authorisation, hold the secret and inject it, so the agent never sees it.
- **Identity platforms**, such as Microsoft Entra Agent ID with Agent 365, and Okta, give agents an identity, an owner and access reviews.
- **Agent frameworks** have approval steps inside one run.

What pgmandate does that none of these does, as far as we have found: a standing grant is a row with a budget, a risk limit, a policy reference and an end date, a decision is a row that names the authority it rests on, and Postgres enforces the rules again, so a bug in the service cannot allow what they refuse. What it lacks: users, high availability and, today, a way in that needs no code. A receiver has to be written for each system; sitting in front of an existing MCP server is next on the [roadmap](docs/roadmap.md).

## Layout

`modules/` holds the domain, `pkg/` the shared plumbing, `schema/` the migrations, `api/` and `mcp/` the two interfaces, `web/` the UI, `examples/` a receiver and an embedded host. [AGENTS.md](AGENTS.md) has the conventions; [docs/decisions.md](docs/decisions.md) has the reasoning.

## Author

Andreas Lindfalk.

## License

Apache-2.0. See [LICENSE](LICENSE). Security reports: [SECURITY.md](SECURITY.md).
