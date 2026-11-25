# Roadmap

Where pgmandate sits, and what would come next. Nothing here is promised; the project is a reference implementation and moves when there is a reason.

## Position

pgmandate answers one question: who allowed this action, under which policy, within which budget, and where is the proof. It does so by taking access away from the agent, not by filtering what the agent does with it.

| Category | What it controls | How pgmandate relates |
|---|---|---|
| MCP gateways and governance proxies | Calls an agent makes with the access it has: allow, deny, ask a person, count spend | The closest neighbours, and easier to adopt, since existing tools keep working. pgmandate overlaps on features and differs on where the gate sits: the agent has no access to filter |
| Cloud and identity platforms | Who the agent is, and allow or deny per call | They own identity. Standing delegation with a budget and a human queue is not their centre |
| Human-in-the-loop libraries and framework approvals | A pause for approval inside one agent run | They return the tool's real result and need no new service. They leave no record across frameworks and have no standing permission |
| Durable execution (Temporal, Restate) | That a workflow finishes | A workflow that waits for an approval needs somewhere for it to live. That is an integration, not a competitor |
| Policy engines (Open Policy Agent) | A yes or no per request | No budget over time, no queue, no record |
| Credential brokers (Infisical Agent Vault, Auth0 Token Vault) | Where the secret lives: injected into the call, never seen by the agent | They answer "who holds the key" and some ask a person; standing permission with a budget and a record of authority is not their centre |
| Payment mandates (Google's Agent Payments Protocol, several 2026 drafts) | Signed authorisations, mostly for purchases | A different problem that uses the same word |

Removing the agent's credentials is by now a common pattern, not a differentiator, and a gateway could add mandates as a feature. What this repository offers is a small, strict, readable implementation: decisions as rows with an authority, standing mandates with a budget and a policy reference, and Postgres as the last gate.

## Next, for this repository

The bar for this repository: one team outside its author runs an agent through it, with the agent's direct credentials removed. What blocks that is the cost of bringing a system in: today every protected system needs a receiver written for it. So the next step is to sit in front of an existing MCP server, and the groundwork that step needs comes first.

The measure: **an existing MCP server governed in ten minutes, with no code.**

### Phase 0: what the MCP front needs

In front of an MCP server, pgmandate holds that server's credential, not just a signing secret it shares with a receiver. A leak of the database would then reach every tenant's systems, and an agent loop that runs away would act at once. Only what guards against that comes first; the rest waits until after the front.

1. **Credentials encrypted under a key outside the database**: a data key per tenant, wrapped by a key-management service, with the tenant and the connector bound into each ciphertext so it cannot be moved. Write-only once stored.
2. **A separate role for the part that sends.** Today any session of the one application role can declare itself the system. The part that decrypts and calls out gets its own database role, which alone may read credentials and execute; the processes that parse what agents and people send cannot, whatever scope they declare.
3. **At most once, with an outcome unknown.** An upstream tool has no idempotency key, so an upstream call is made once. A call that times out gets a delivery status of its own, "outcome unknown", for a person to settle as done or not done, never retried blindly. The statuses are designed together.
4. **Expiry and withdrawal** of a waiting proposal, with a stated outcome, and a **callback to the agent's host** when a decision is made, so an agent that ended its run can resume without polling.
5. **Limits per agent**, per minute and per hour, held in the database so they hold across instances: a runaway loop must not spend a month's budget in seconds. Limits per run do not count as a control, since the agent chooses its run id.
6. **How pgmandate authenticates upstream.** Most remote MCP servers authorise a user through OAuth. pgmandate needs a client, token storage and refresh, and an answer to whose identity the upstream server sees: one service identity widens what an agent can reach and hides the person from the upstream audit. Credentials are scoped per agent or per mandate, not one per tenant. The address checks apply to discovery, token and callback endpoints too, and the callback is set by an owner, never an agent.

### Phase 1: in front of an existing MCP server

The agent connects to pgmandate, and pgmandate presents the upstream server's tools. A call to a tool that changes something becomes a proposal; once it is approved or allowed by a mandate, pgmandate makes the real call and the tool's answer is the result.

- **Remote MCP servers only.** Running a local (stdio) server inside pgmandate would make it a sandbox for other people's code, which is not its job.
- **Mapping.** An owner maps upstream tools to actions: key, risk, cost, and the tool's own input schema as the payload schema. A hash of each mapped tool's definition is kept, and a tool whose definition changes upstream is refused until an owner maps it again. Upstream descriptions and results are untrusted text. An approval carries the digests of the payload and of the tool definition the person saw.
- **The idempotency key, to decide.** Upstream tools have no field for it. The likely answer is a key in the call's `_meta`, which MCP allows on any request, with the presented tool schema gaining a required key where a client cannot send `_meta`. Guessing a key from the content is ruled out, as for proposals today.
- **Reads, to decide.** Passing a read through means the credential holder makes a call with no decision behind it, which the separate role in phase 0 is there to prevent; not offering reads means the agent needs its own read credential for the same server. The default is not to offer them, and the documentation says what that costs.
- **The answer while a person decides** is a normal tool result that says `pending` with the proposal's id, worded so a model cannot read it as done. A call is never held open for hours. MCP's long-running tasks are an option once they settle.
- **Typed tools**: each action, native or upstream, presented as a tool with its own input schema, so a model and a client's strict mode see the shape where they look for it.

This lands pgmandate among the MCP gateways, which already do much of it. What it brings there is what it has now: a decision is a row with an authority, a standing mandate with a budget, and the database enforcing both.

### Phase 2: conditions on a mandate

A mandate today sees the action, its risk, its cost and the budget, never the content. People should see only what they need to, and that needs rules about the content.

1. **First, conditions the database can check**: comparisons on payload fields (`/amount <= 500`, `/currency in [EUR]`) and a cost taken from a payload field, compiled into the same check that verifies a mandate's basis. Postgres stays the last gate. A cost from the payload replaces two rules that assume a fixed cost per action (the insert check and "the action changed"), which are redesigned with it.
2. **Then CEL**, for what comparisons cannot say. It runs in the service, so the database can check only that the condition cited is the mandate's own and unchanged; the threat model will say so. Errors, missing fields and wrong types mean "not allowed", evaluation has a cost limit, and a condition can only narrow what the mandate allows.
3. **A refused outcome** for what must never happen, beside allowed and waiting.
4. **Why, and why not.** The basis names the clause that held; a waiting proposal names the clause that failed. A recorded dry run lets an agent ask before proposing.
5. **Limits on splitting**: a condition on the total under a mandate in a window, so a large action cannot be split under a threshold.

Facts about the world (prices, positions, opening hours) stay with the system that has them. A standing order such as a stop loss belongs at the system that executes it, approved once by a person, not proposed by an agent when the moment comes.

## What a product would need

The larger aim is one place where every action an organisation's agents take is proposed, decided and recorded, whatever built the agent and whatever system it changes. From one author with no users that is a direction, not a claim. A product would need, beyond the phases above:

- **Every kind of agent:** a plain HTTP API with small TypeScript and Python clients, and hooks in agent frameworks that make each tool call a proposal without blocking the run: approvals in the OpenAI Agents SDK and LangGraph's interrupt (both can suspend and resume), a Temporal activity that proposes and waits for a signal, and the Claude Agent SDK's pre-tool hook for the fast path.
- **An inventory**: every agent, what each may do, what it spent, how much ran without a person and how much waited.
- **Suggested mandates** from the record, such as "approved unedited 97% of the time last month under 500; grant a mandate?". The evidence shows edits and rejections first, needs a minimum number of decisions by more than one person, and leaves out approvals made in a rush; a backtest shows what the rule would have allowed, including what a person rejected. The suggestion opens the ordinary grant dialog filled in, never a one-click grant, and a dismissal is recorded. The same evidence suggests narrowing or revoking: a budget never used, rejections rising, deliveries failing, an end date coming. Rules on amounts need the payload conditions of phase 2. As of November 2026 this is the one idea here not found shipped elsewhere; OpenLeash, where agents propose policies to their owner, comes closest.
- **Exports**: decided proposals as a dataset for evaluating agents, a feed for a security monitoring system, and evidence packs for AI Act or ISO 42001 work.
- **People where they are:** approving in Slack or Teams, as another door to the same record. A chat user is tied to a person by user and workspace id, never by name; buttons are bound to a proposal and its payload digest and stop working when the action changes; the message leads with what the action does and marks the agent's text as the agent's, and is updated when anyone decides elsewhere. Edits and high risk go to the web. Two approvers for high risk, on the web and in chat alike, the second distinct from the editor and shown the edit. Who is notified follows the action or department, with a fallback when someone is away.
- **The inbox as a triage tool:** grouped by why proposals wait, oldest first, with filters by agent and action, a "mine" scope, a count of what was allowed today with spot checks, and a recurring reason leading to a rule change rather than to forty approvals. No bulk approve. The tab in the address.
- **The rest of the groundwork:** deliveries signed with a key pair, so a receiver holds only a public key; results larger than 4 kB, kept for a stated time; erasure of personal data from a payload while the log, which holds only digests, still verifies; OpenTelemetry spans; the log's latest hash published outside the database; a fair share of delivery workers per tenant; people identified by their provider's subject id; limits on failed sign-ins and bad tokens.
- **Departments as scopes**, single sign-on with user provisioning, and passkeys.
- **Not a choke point.** Putting every agent action through one service makes it a single point of failure, which no organisation will accept as a condition. Failing closed is right for changes, as when a person is away, and reads do not pass through it. Several instances behind a load balancer with a replicated Postgres, a latency budget, pagination and streaming exports, a narrower lock on the log, and a sweep for stranded deliveries come before anything depends on it. And it stays optional per system: an organisation puts pgmandate in front of what is worth governing.

## Deliberately not

Sandboxes for agent code, agent memory, messaging between agents, caching and model routing. They are infrastructure for running agents; pgmandate governs what agents change. A general gateway is not the aim either: pgmandate sits in front of an MCP server to turn its changes into proposals under mandates, not to route, cache or rate limit traffic.

**Signed mandates an agent carries.** They were considered for keeping low risk actions going while the service is down. They would give the agent a credential again, they cannot be revoked at once or counted against a shared budget while offline, and in front of an MCP server nothing runs without pgmandate anyway.

## How it would matter

One team outside its author runs an agent through it, with the agent's direct credentials removed.
