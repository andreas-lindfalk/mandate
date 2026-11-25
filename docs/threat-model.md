# Threat model

What pgmandate protects, against whom, and what it does not. What it is for, more broadly, is in [scope.md](scope.md). Written to be read before anyone relies on it.

## What it is for

An agent that has only a mandate token cannot change anything except by proposing. Whether the proposal is carried out is decided by a person, or by a mandate a person granted, and both leave a record that names the authority. A dry run, once built (see the roadmap), is outside this: there the agent still holds its own credential, and pgmandate only records.

## What holds, and where

**Against an agent with its token.** What it may write is enforced by the service and by the database. What it may read is enforced by the service only: the database separates tenants, not agents within one.

- It can propose, withdraw its own waiting proposal, and read its own proposals and the actions. Nothing else.
- It cannot approve, edit or reject, grant or define. The database refuses these too.
- It cannot read another agent's proposals or the people: the MCP server withholds them, though a transaction scoped as an agent could read them. It cannot read connector secrets at all: the application role may write that column but has no right to read it, and only a transaction scoped as the system gets a secret, through `connector_of`, together with the address it belongs to.
- It cannot understate the risk of a defined action with a webhook connector, choose how long its proposal waits, propose a removed action, or propose after it is revoked.
- Its arguments are bounded, and its title, payload, rationale and evidence stay on the proposal row, out of the permanent log, which holds their digests. Two things it chooses do reach the log as written: its run id, a label of up to 100 characters on every event of the run, and its provenance labels (client, model, agent version), names of up to 100 characters in a narrow alphabet. An agent could put a name in either that can then never be erased.
- It cannot make more proposals than its limits per minute and per hour; the database refuses the rest, whichever server instance it calls.
- A third thing it chooses reaches the log as written: the action key it names in a tool call, when it has the shape of a key, even for an action that does not exist.
- It cannot send a payload with a key a receiver could read as another (two keys that differ only in case, or a letter some decoders fold into ASCII): the service and the database refuse it, so a condition is never checked on one key and carried out on another.
- It can withdraw its own waiting proposal, and nothing else changes with it.
- Its rationale and evidence are its own claims. They are shown to the person deciding, labelled as the agent's, and never read by a mandate. An agent that lies in them misleads a person, not the rules.

**Against a bug in the service.** Enforced by the database, with tests that bypass the Go code.

- Only an agent's session inserts a proposal; only a person of the tenant decides; an edit exists only as part of an approval, and a proposal left waiting cannot be changed at all.
- A proposal becomes "allowed by a mandate" only for the system, inside the transaction that names that proposal, with a basis that matches a live mandate row: tenant, agent, action coverage, risk limit, policy reference, budget and this month's spend, recounted under a lock, and the mandate's condition, which the database evaluates itself on the payload executed. A condition that cannot be evaluated (a missing field, another type) does not allow.
- A defined action that is not marked as delegable is never allowed by a mandate, and neither is a proposal whose webhook action's risk was raised since it was proposed.
- A proposal past its deadline is decided by nobody: the database refuses an approval, a rejection, a withdrawal or an execution, and lets only the system expire it. Before the deadline, nothing expires it. The comparison uses the transaction's start, so a decision whose transaction began before the deadline is in time.
- A delivery is recorded only in the transaction that decides its proposal, only to its action's own connector, and at most one per proposal. The approval names the connector it was made against. A delivery waiting or failed follows its action, though: when an owner moves the action, or a person sends a failed delivery again, it goes to the connector as it is then, which the approver may never have seen. Both are in the log (`delivery.retargeted`, `delivery.retried`), and the delivery event names the address it was sent to.
- A webhook moved to a new address gets a new secret; the table refuses the move otherwise.
- Append-only rows are never changed, and leave only through `purge_tenant`, which only the database owner can call. The guard lets a delete through only for the table's owner with the purge setting on, so a delete right granted to the application role by mistake still removes nothing (`mandate.forbid_change`, in `schema/0001_init.up.sql`).
- Every proposal has an idempotency key, unique per agent, and neither the key nor the hash of what was asked for under it can be changed. A repeated request cannot become a second proposal.
- Every log event carries the actor and the run of its session and must continue the chain: the next number, the last hash, and a time no earlier than the event before and no later than the database's clock (or equal to the event before, while a clock stepped back catches up). The service takes that time from the database's clock; the database checks only that it falls in that window.
- The payload as the agent proposed it is kept beside any edit, set by the database and never changed.
- People, agents, mandates and actions are changed only by an owner. Revoking an agent, a token or a mandate and removing a person are final, and the last owner cannot be removed, even when two owners remove each other at once. A removed person can be added again as a new person, and a removed action can be defined again.
- A mandate's condition is evaluated by the database on the payload executed, and the basis must cite the condition as granted.
- Every new proposal has a rationale, and the rationale, the evidence and the provenance are never changed, by a person deciding either. Evidence and provenance are held to their shape.
- An agent's proposals beyond its limits are refused, counted under a lock so that concurrent proposals cannot pass them.
- A notifier's address is written by an owner and read only by a transaction scoped as the system, through `notifier_of`; the application role may write the column but has no right to read it. It must be a Slack or Discord incoming webhook address.
- A condition's plan is held to the shape the compiler writes: known operators, paths into the payload or the action, number, string or boolean literals, bounded in depth and size.
- Only the agent that made a proposal withdraws it, only while it waits, and nothing else changes with it.
- The budget and the agent's limits are counted after a lock, which holds only under read committed; an execution or a proposal under another isolation level is refused rather than counted wrongly.
- A payload with an ambiguous key is refused (the rule applies to payloads stored from that migration on).
- One tenant never sees another's rows, and the triggers filter by tenant themselves rather than relying on row level security alone.

**Only in the service.** The database does not check these.

- That an approval runs at the action's current risk when it is higher than the proposal's. The database lets an approval raise the risk; it does not require it.
- That a reused idempotency key with another action or payload is refused, and that the hash kept beside a key is the hash of that request. The database keeps one proposal per agent and key and keeps both columns unchanged; the comparison is made by the service.
- The payload against the action's schema, when it is proposed, edited, approved and allowed by a mandate. A schema changed after the decision does not reach back to a delivery already recorded.
- The steering-text tripwire and the flag it sets, which reads the rationale and the evidence too.
- That a notification carries no text the agent wrote: the service builds it from the action, the risk, the agent's registered name and the reason, and escapes them for the chat tool.
- The risk assessed from a payload by an embedded host's own code.
- The explicit confirmation before approving a high risk proposal.
- That an action which exists only as code in an embedded host may be delegated at all.
- What an event says. The database checks that each event continues the chain and carries its session's actor and run. It does not check the event's hash, the digests in it, or that a decision writes an event at all. The service writes them: the proposal, approval and execution events carry a digest of the payload, and a delivery event a digest of the exact body sent. A payload rewritten in the table later therefore no longer matches the log, for anyone who checks.
- That an agent reads only its own proposals. The database separates tenants, not agents within one.
- That a mandate's stored plan is the compilation of its condition's text. The service compiles it at the grant and the table keeps both unchanged; the database evaluates the plan, not the text.

## What it does not protect against

- **An agent that still has its own credentials.** pgmandate governs what goes through it. Removing the direct access is the precondition, and it is outside this software.
- **Arbitrary SQL as the application role.** The database trusts the scope a transaction declares: tenant, actor, and the proposal being executed. Code that sets it honestly is held to the rules. An attacker who can run their own SQL as `mandate_app`, through an injection or a compromised server, can declare any scope and is not held. The guarantee is about bugs, not about a compromised process.
- **The database owner.** The owner bypasses row level security, can drop triggers, and can rebuild the log. Run `serve` with `MANDATE_APP_DATABASE_URL` so the server never holds the owner's credentials, and keep the log's latest hash somewhere else.
- **Reads.** What an agent reads, and where it sends it, is not seen.
- **An agent that can act as a person.** Locally, dev login lets anyone on the machine sign in as any email, the command line's `-as EMAIL` acts as any person, as the application role, from the owner connection or `MANDATE_APP_DATABASE_URL`, and the default passwords are in this repository. An agent with a shell or network access on the same machine can approve its own proposals. The local setup is for trying things out; run the agent as another user or in a container, and run pgmandate as described under "Running it" before an agent depends on it.
- **Whoever holds a connector's secret.** It is held by pgmandate and by the receiver. Either can sign a body the receiver accepts, with no proposal behind it. The secret changes when the action's URL changes, and the deliveries still waiting move to the new URL, so a receiver the action moved away from gets nothing more, apart from an attempt already in flight at that moment. Rotate it by hand when a receiver is retired some other way.
- **Any session of the application role declaring itself the system.** The role is one; the scope a transaction declares is trusted. Code that declares the system scope wrongly can read connector secrets through `connector_of` and execute under a mandate. A separate role for the part that sends is on the roadmap.
- **A careless approver.** A person who approves everything is not oversight. Many approvals in a minute are written to the log; nothing stops them.
- **Injection in general.** The tripwire catches a few English phrasings. A paraphrase, another language or an encoding passes. That includes the rationale, which a person reads: a rationale can be written to persuade.
- **Claims in a payload.** A condition decides on the payload as the agent wrote it. Whether a claimed fact (an order's age, a customer's status) is true is for the receiver to check in its own records before it acts.
- **Splitting under a condition.** A condition limits each proposal, and an agent sees the condition's text in list_actions. A large refund can be proposed as many small ones that each pass, up to the mandate's budget, which counts executions, not the amounts in them. A budget in money, read from the payload, is on the roadmap; until then, a mandate whose condition limits an amount needs a budget that bounds the sum.
- **What a receiver tells the agent.** A receiver's reason for refusing is kept and shown to the person and the agent. A receiver that names what its records hold in that reason lets an agent learn them by trying claims; `examples/refunds` names only the claim that failed.
- **Whoever holds a notifier's address.** Anyone with it can post to the channel. It is kept like a connector secret, and replacing the notifier is how it is rotated.
- **A channel flooded.** Every proposal left for a person sends one message, up to the agents' limits. A runaway agent within its limits can still fill a channel; there is no summary or cap per tenant yet.
- **A malicious owner of a tenant.** An owner can grant anything up to medium risk and point an action at any public address. Connectors on local and private addresses, and plain http, are refused unless the operator allows them, and redirects are never followed, so the server cannot be made to call into its own network or send a decision across the internet unencrypted.
- **One tenant slowing another.** Deliveries have their own job queue, so a slow connector does not hold up decisions. But all tenants share that queue and its workers: one owner who points actions at a connector that never answers delays every tenant's deliveries.

## Known gaps

- Connector signing secrets are stored unencrypted, because the server signs with them. The application role cannot read them except as the system, but the database owner, a backup or a copy of the disk can.
- Notifier addresses are stored unencrypted, like connector secrets, and are in every backup.
- A condition's text is written by an owner and kept in the log as written. An owner who puts personal data in a condition cannot erase it.
- The log's digests are SHA-256 without a salt. They show that content changed; they do not hide short or guessable content (an order id, a short note) from someone who can read the log and guess it.
- A payload or a rationale cannot be erased. Proposals are append-only, and only removing the whole tenant deletes them, so a request to erase personal data in one proposal cannot be met. Agents are told to refer to people by id, which helps only as far as they do.
- A person is identified by the verified email address from the sign-in provider, not by the provider's subject id. The provider must serve a single organisation.
- Sessions last a week by default and are not tied to the provider's session. A session is by email and spans tenants: removing a person from a tenant ends their access to it on the next call, and leaves their other tenants alone. Disabling them only at the provider does not end a session until it runs out.
- There is no rate limit on failed sign-ins or bad tokens. The limit on an agent's calls is kept in each server's memory, so several instances each allow the full rate.
- An agent that sends the same request under a new idempotency key has made a second proposal. Nothing compares the content of proposals.
- What a person does through the API is not idempotent: a grant sent twice is two mandates, each with its own budget. The web UI disables the button while a call runs; the API has no key for it.
- If the final failure of a delivery cannot be recorded, the row stays pending; nothing reconciles deliveries against the job queue.
- A connector on an address that may not be called is accepted when the action is defined and refused when the first delivery is attempted.
- The reason a proposal waits is written to the log when a mandate was tried and did not allow it. For a proposal that could never be delegated (flagged, high risk, or an action that is not delegable) the reason is worked out when it is shown, not recorded.
- Lists return the newest few hundred rows, and exports are built in memory. Neither will hold for a large tenant.
- The log is one lock per tenant. Every agent call writes to it, so a busy tenant serialises on it.

## Not reviewed

There has been no external security review. The service was built with an AI coding agent under its author's direction, and reviewed by its author and by several rounds of AI reviewers taking different roles (staff engineer, security, agent developer, interface, positioning, first-time user). No independent person has tried to break it. Treat it accordingly.
