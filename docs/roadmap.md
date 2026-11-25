# Roadmap

What comes next, in order, and why. What pgmandate is and is not is in [scope.md](scope.md), and the roadmap stays inside it. Nothing here has a date.

## The aim

pgmandate should be useful to teams other than its author's. The measure stays as it was:

**One team outside its author runs an agent through it, with the agent's direct credentials removed.**

That depends more on one real team than on more features. So the roadmap has three tracks:
- **Finding a first team** runs from the start, alongside phase 1.
- **Phase 1** makes pgmandate easy to try and safe to pilot, and fixes the contracts others will build on.
- **Phase 2** is a list to choose from. What gets built, and in what order, follows what that team runs into.

Rough size, for one person working evenings with AI agents: phase 1 is three to four months. If it runs long, two items wait:
- item 4, one version everywhere
- MCP on the agent contract in item 9. HTTP comes first, and today's MCP server keeps working until then

Phase 2 is more than anyone needs; a first team will need a part of it.

**How an item is built.** The roadmap says what each item must achieve and what it must not break. Before any code, an item gets a short design note in `docs/`, reviewed like code. The note covers its states, its migration, its contract changes and its tests. Items 5, 6, 8 and 9 shape the contracts and the schema that 0.1.0 freezes, so they are designed together, before any of them is built: [design-0.1.md](design-0.1.md).

**No compatibility before 0.1.0.** Nobody runs pgmandate yet, so until 0.1.0:
- the schema, the migrations and the contracts change freely
- the migrations may be folded into one fresh first migration
- a 0.0.1 database is reset, not upgraded

From 0.1.0 on, compatibility holds:
- **Migrations.** A migration that has reached `main` is never changed.
- **Existing rows.** Rows that already exist at an upgrade get a stated, fixed treatment, never whatever a new column defaults to.
- **Data changes that write to the log** run as a step in Go after the schema migration. Such a step runs as the system, can be run again safely, and has a test that the chain still verifies after the upgrade. A SQL migration cannot write a correct log event.

## Finding a first team

This is the critical path, so it starts at once.
- Talk to teams that run agents which change real systems.
- Write down what they would need in order to try pgmandate, and feed it into phase 2.
- Publish a write-up of the design, the database as a second gate and the mandate as a grant, so that the right teams can find the project.

**Checkpoint when phase 1 is done.** If no team is in sight, phase 2 waits, and this roadmap is reviewed.

## Phase 1: ready for a first team (0.1.0)

### Easy to try

1. **One image and a ten-minute quickstart.**
   - Publish an image with the UI built in, plus a quickstart that needs only Docker.
   - Dev login stays limited to a server that listens only on the machine itself. A container listens on every address, so the quickstart needs another way in: the server prints a single-use sign-in link for the first owner. The link works only while the tenant has no owner, expires quickly, and is stored hashed, since container logs are an admin path.
   - Seeing the UI then needs neither Go, Node nor an OpenID provider.
2. **The README as a front door.** It opens with what the scope says, and has a drawn diagram and a recorded demo of the whole loop. The long reference sections move into `docs/`.
3. **Database setup that works without a superuser.** On managed Postgres (RDS, Cloud SQL, Azure, Supabase, Neon) the admin role is not a superuser, and setting a role's attributes fails. Setup therefore:
   - checks the application role's attributes and refuses to go on if they are wrong, including when the role owns pgmandate's tables, or has SUPERUSER, BYPASSRLS or CREATEROLE, or can switch through membership to a role that does, such as a managed service's admin role (`rds_superuser`, `cloudsqlsuperuser`). Each of these gets past row level security or the append-only guard
   - can skip creating the role when a DBA created it beforehand
   - revokes `CREATE` on `public` only when asked to
4. **One version everywhere.** The binary, the MCP server and the command line's own MCP client all report the release they were built from, and `pgmandate version` prints it. Today the server says `0.1`, the client says `1` and calls itself `mandate-cli`, and that name reaches the log as provenance.

### Safe to pilot

5. **Proposal expiry.**
   - An action sets how long a proposal for it may wait, and how long a decision may take to be carried out. Both are fixed on the proposal when it is made.
   - When the waiting time is up, the system decides the proposal as expired under a database rule, the agent is told, and nothing is delivered.
   - After that deadline, the database refuses every change to the proposal except expiry: no approval, no withdrawal, and no execution under a mandate.
   - A decision can still be delivered late: retries, an outage, or a person sending a failed delivery again. So the delivery carries `notAfter` (item 8): the time of the decision plus the action's window. After it:
     - the worker stops retrying
     - the database refuses a person's resend
     - a receiver refuses the delivery
6. **A dry run, as the way in.** Built (build step 4 of [design-0.1.md](design-0.1.md)). No team removes an agent's credentials on day one.
   - **Setting it.** A new agent starts in dry run, and an owner makes it live, or creates it live. The switch is logged. Each proposal records whether it was made in a dry run. The database sets that, and it never changes, so turning dry run off later does not turn old proposals into real ones. Every answer to the agent says whether it is a dry run, so a key reused after the switch can never return a dry-run proposal as if it were real.
   - **The agent's side.** In a dry run the agent keeps its credential. It proposes what it is about to do, and carries on without waiting.
   - **pgmandate's side.** It records what a mandate would have allowed and what would have waited for a person.
   - **Its own outcomes.** A dry run never becomes *executed*. The database refuses to deliver it, to let it count as spend, or to let `may_write_for` open a host's table for it, so it cannot reach a receiver or an embedded host's code.
   - **No receiver needed,** and a receiver that does not know about dry runs can never act on one.
   - **What people see.** Waiting dry-run proposals show in the inbox, marked as such. A person may dismiss one, never approve it, and they send no chat notice.
   - **In the record.** Dry runs stay in the log, tagged, so the chain and its export stay whole. They are left out of the decisions export and the figures on how often people agree.
   - **What it gives a team.** It sees what its mandates would allow, and what people would be asked, before it takes the key away. Principle 1 in the scope names this as its one exception.
7. **Sign-in that holds up.**
   - People are identified by their provider's issuer and subject id together, not only by email. The binding is per tenant, so a later sign-in per organisation needs no second migration.
   - A person not yet bound is bound at their first sign-in to that tenant, by verified email, once. Once bound, the same email under another issuer and subject is refused in that tenant.
   - Failed sign-ins and bad agent tokens are limited.

### Contracts fixed before others build on them

8. **Delivery contract v2.** Built (build step 2 of [design-0.1.md](design-0.1.md)). Receivers, templates and the conformance test are written against it, so it comes first. It has to hold what phase 2 adds without a v3.
   - **Signed with a key pair (Ed25519).**
     - The signing key is kept outside the database, in an environment secret or a key management service. A receiver holds only the public key, so neither a receiver nor a copy of the database can forge a delivery.
     - The signature carries a key id, for rotation, and an audience. pgmandate assigns the audience per tenant and connector, and signs it. Moving an action to a new address gives it a new audience.
     - A receiver is configured with the tenant and the audience it accepts, and never takes them from the request. A body signed for one receiver, or for another tenant, is not valid there.
     - The per-action shared secrets go, and the threat model's rules for them are rewritten for audiences.
   - **A list of budgets, each with a kind:** `budgets: [{kind: count | money, limit, spent, currency}]`.
     - Phase 1 counts executions. "Units" and an action's fixed cost go, from the schema, the contract and the UI.
     - Principle 3's rule on a changed action is restated for what remains: a webhook action's raised risk, and later the cost fields of a money budget.
     - Money (phase 2) adds one block per currency to the list, not a new version.
   - **`notAfter`** (item 5).
   - **A kind in what pgmandate signs:** a delivery or a callback, so one can never pass for the other, whatever the audiences are. A late result is the receiver's, and is authenticated only by its token (below).
   - **Only an explicit answer is final.** A receiver says `done`, `refused` or `unknown`. Anything else is retried, and ends as *unknown* if an attempt was sent, or *not sent* if none was. The worker records that an attempt started before it calls, so a crash cannot turn "maybe" into "not sent".
   - **A person settles *unknown*** as done or not done, and only once the work can no longer happen. A delivery may be sent again only from *not sent*, and only before `notAfter`. Only after "not done", "refused" or a late "not sent" may the agent propose the same thing again.
   - **A result reported later** (an *accepted* answer) is left out of 0.1.0. It is purely additive, so it can come later without a new contract.
   - The details are in [design-0.1.md](design-0.1.md).
9. **One agent contract.** Built (build step 3 of [design-0.1.md](design-0.1.md)).
   - The agent's operations are list actions, propose, get, list and withdraw. They move out of `mcp/` into one service defined as a Connect contract (`mandate.agent.v1`).
   - MCP becomes one way to call it, and JSON over HTTP comes with it. The clients in phase 2 are generated from it.
   - The call limit, the waiting and the guidance texts live there once.
   - What only the service holds today moves with it, with tests: an agent reads only its own proposals, and the agent service refuses a person's session.
10. **The job queue kept apart.** pgmandate's River jobs live in a schema of their own, with their own migrations and queue names. The application role gets rights there and nowhere in a host's `public` schema. What a host must know about sharing one River version in one binary is written down.
11. **What stays flexible, as tests.** The assumptions under *What stays flexible* in the scope become CI that runs:
    - setup as a non-superuser owner
    - an embedded host that runs its own River
    - two server instances on one database. The expected result: decisions, the log and the per-agent proposal limits hold across both, and the per-second call limit holds per instance, as the threat model says.

**Done when:**
- With only Docker installed, a newcomer gets from a clone to a decided proposal in ten minutes.
- `examples/refunds` runs against contract v2.
- The demo shows proposal expiry and a dry run end to end.
- The tests in item 11 pass.
- 0.1.0 is released.

## Phase 2: what a first team needs

Each item below has a reason to exist. Which ones get built, and when, follows what the first team uses and runs into.

- **Clients in Python and TypeScript**, generated from the agent contract.
- **One framework adapter, for the first team's framework, in a form that governs.**
  - The governed tool's body proposes and waits for the receiver's result. The agent's process never holds the system's credential.
  - Waiting hours for a person is what durable execution does well. Where a team runs Temporal or Restate, the natural form is an activity that proposes, with the decision arriving as a signal through the callback below.
  - An adapter that only answers a framework's approval hook (`needsApproval`, `canUseTool`, `interrupt`) pauses a tool that then runs with the agent's own credential. That is recorded approval, not governance, and it is offered, if at all, under that name.
- **A callback when a proposal is decided**, so a run that suspended resumes without polling:
  - An owner registers the address per agent; an agent never chooses where pgmandate calls. The address is held to the same checks as a connector's.
  - It is signed with the deployment's key, under an audience of its own, so a callback can never pass for a delivery.
  - It carries only "proposal X changed"; the host reads the rest with its own token.
- **A receiver template in Python**, and in TypeScript if the team needs it, plus a conformance test any receiver can run. The test covers:
  - the signature, the key id, the audience and the time window
  - `notAfter`
  - a repeated delivery having no second effect
  - a refusal shaped so pgmandate keeps it
  - outcome unknown
- **A plain webhook notifier**, so any chat tool or paging system hears when something waits. It still carries none of the agent's text.
- **Notifier addresses encrypted** under a key outside the database. They are bearer capabilities, and they sit in every backup today.
- **Budgets in money**, as a second limit beside the count.
  - The limit is an amount per currency, with no exchange between currencies.
  - An action names the payload fields that carry its cost and currency. Those names are fixed on the proposal when it is made, so an owner cannot remap them before a decision. The database reads them on the payload executed, as it does a condition.
  - A cost that is missing, negative, not a number, or in a currency the mandate has no budget for waits for a person.
  - The cost field must be the field the receiver carries out. Otherwise an agent could state a small cost and have a large amount paid. The conformance test checks it, and the threat model's "cannot understate the cost" is restated for it.
  - It caps an agent splitting a large request into small ones at the month's budget. It does not stop the splitting.
- **A bridge to an existing MCP server,** built when a team has a remote MCP server to govern. It is a receiver, run next to the system, that carries out decided proposals by calling the server's tools with a credential only the bridge holds. What it needs:
  - **Outcome unknown, from contract v2.** Upstream tools take no idempotency key, so a call is made once. The bridge records a delivery id durably before it calls. A delivery it has seen but not finished, or a call that timed out, is outcome unknown.
  - **Upstream authorization,** usually OAuth.
  - **Its own identity.** An owner issues the bridge a credential of its own, which can read actions and propose mappings, and nothing else.
  - **Mapping.** The bridge reads the server's tools and *proposes* a mapping. An owner confirms it in pgmandate: key, risk, and the tool's input schema as the payload schema.
    - The database keeps a bridged action from being delegated until an owner says otherwise, so the bridge cannot raise its own authority.
    - A hash of each mapped tool's definition is kept. A tool whose definition changes upstream is refused until it is mapped again.
    - The bridge refuses tools that are not mapped. The documentation asks for an upstream credential scoped to the mapped tools.
  - **No reads.** Passing reads through would let the credential holder make calls with no decision behind them.
  - **What it reports.** The bridge returns the tool's name, its definition hash and a digest of the arguments with the result, so the log shows what was called, not only that a delivery went out. Upstream descriptions and results are untrusted text.
  - **Mandates on bridged actions.** The bridge cannot check claims, since it has no records of its own. A mandate on a bridged action therefore rests on what the agent wrote, which is why bridged actions start with a person deciding.

## Later, when someone needs it

Each of these is real, but none blocks a first outside team. They are listed so that nothing done earlier closes them off.

- **Embedding:**
  - the server as an importable package
  - creating a tenant with the host's own id
  - a hook for the host's own session variables
  - the application role as a group role, decided together with a separate role for the part that delivers; this needs care, since a host's whole application would then act with pgmandate's rights
- **People:**
  - more channels: Microsoft Teams and email
  - routing by action or department
  - approving from the chat, bound to the payload's digest, never tied to a person by name
  - two approvers for high risk
  - the inbox as a triage tool: grouped by why proposals wait, filters, a "mine" view, and no bulk approve
  - departments as scopes
  - passkeys
  - an idempotency key on the people API, so a grant sent twice is one mandate
- **Conditions:**
  - the rest of CEL, evaluated by the service, with the database checking that the condition cited is the mandate's own
  - a *refused* outcome for what must never happen
  - a recorded check an agent can ask for before proposing
  - limits on a total within shorter windows
- **Agents:** typed tools, each action offered as its own MCP tool. Action keys contain dots, which some clients refuse in tool names, and the payload must sit under a field of its own so it cannot collide with the rationale or the idempotency key.
- **The record:**
  - an inventory: every agent, what it may do, what it spent, how much ran without a person
  - suggested mandates from past decisions, never a one-click grant
  - exports as an evaluation dataset. Every decision is a person's verdict on an agent's proposal: approved unedited, edited, or rejected, by model and version. That is a labelled set for evaluation tools such as Langfuse, Arize Phoenix or Braintrust.
  - a feed for security monitoring, and the IETF agent audit trail format
  - the log's latest hash published outside the database
  - the database checking that each decision writes its event, and the event's hash
  - erasure of personal data from a payload while the log still verifies
- **Scale, and not a choke point:**
  - Failing closed is right for changes, and reads never pass through pgmandate. It stays optional per system: a team sends through it only the changes worth governing.
  - several instances with a replicated Postgres, and a latency budget
  - pagination and streaming exports
  - a narrower lock on the log
  - a fair share of delivery workers per tenant
  - per-agent call limits held across instances
  - OpenTelemetry and metrics
  - a Helm chart
  - written guidance on TLS, connection poolers (River needs a direct connection or poll mode), backup and restore (the role first) and upgrades
- **Hosted for several organisations:**
  - sign-in per organisation
  - sign-up
  - a signing key per tenant
  - a way for receivers behind a firewall to fetch decided proposals instead of being called; the signed body does not depend on how it travels
- **Other integrations:**
  - a policy engine hook, so a mandate can also ask Cedar, OPA or Cerbos
  - a bridge for REST APIs described by OpenAPI, on the terms of the MCP bridge
- **Documentation:** a mapping to the EU AI Act's articles on logging and human oversight (12, 14 and 26) and to the OWASP top 10 for agentic applications, as practice that supports those controls, not as a compliance claim.

## Deliberately not

- **A gateway or proxy in pgmandate itself.** Putting pgmandate in front of MCP servers was the plan until November 2026. The bridge replaces it: the credential stays next to the system, and pgmandate stays out of the gateway market, where others are ahead. See [decisions](decisions.md).
- **Storage other than Postgres, and a Postgres extension.** The database as the second gate is the point. Plain migrations run on managed Postgres; extensions often do not.
- **Signed mandates an agent carries.** They would give the agent a credential again, and they cannot be revoked at once or counted against a shared budget offline.
- **Infrastructure for running agents:** sandboxes for agent code, agent memory, messaging between agents, caching and model routing. pgmandate governs what agents change.
