# Threat model

What pgmandate protects, against whom, and what it does not. Written to be read before anyone relies on it.

## What it is for

An agent that has only a mandate token cannot change anything except by proposing. Whether the proposal is carried out is decided by a person, or by a mandate a person granted, and both leave a record that names the authority.

## What holds, and where

**Against an agent with its token.** What it may write is enforced by the service and by the database. What it may read is enforced by the service only: the database separates tenants, not agents within one.

- It can propose, and read its own proposals and the actions. Nothing else.
- It cannot approve, edit or reject, grant or define. The database refuses these too.
- It cannot read another agent's proposals or the people: the MCP server withholds them, though a transaction scoped as an agent could read them. It cannot read connector secrets at all: the application role has no right to that column, and only a transaction scoped as the system gets a secret, through `connector_of`, together with the address it belongs to.
- It cannot understate the cost of a defined action, or the risk of one with a webhook connector, propose a removed action, or propose after it is revoked.
- Its arguments are bounded, and its title and payload stay on the proposal row, out of the permanent log, which holds their digests. One thing it chooses does reach the log as written: its run id, a label of up to 100 characters on every event of the run. An agent could put a name in it that can then never be erased.

**Against a bug in the service.** Enforced by the database, with tests that bypass the Go code.

- Only an agent's session inserts a proposal; only a person of the tenant decides; an edit exists only as part of an approval, and a proposal left waiting cannot be changed at all.
- A proposal becomes "allowed by a mandate" only for the system, inside the transaction that names that proposal, with a basis that matches a live mandate row: tenant, agent, action coverage, risk limit, policy reference, budget and this month's spend, recounted under a lock.
- A defined action that is not marked as delegable is never allowed by a mandate, and neither is a proposal whose action's cost has changed, or whose webhook action's risk was raised, since it was proposed.
- A delivery is recorded only in the transaction that decides its proposal, only to its action's own connector, and at most one per proposal. The approval names the connector it was made against. A delivery waiting or failed follows its action, though: when an owner moves the action, or a person sends a failed delivery again, it goes to the connector as it is then, which the approver may never have seen. Both are in the log (`delivery.retargeted`, `delivery.retried`), and the delivery event names the address it was sent to.
- A webhook moved to a new address gets a new secret; the table refuses the move otherwise.
- Every proposal has an idempotency key, unique per agent, and neither the key nor the hash of what was asked for under it can be changed. A repeated request cannot become a second proposal.
- Every log event carries the actor and the task of its session and must continue the chain: the next number, the last hash, and a time no earlier than the event before and no later than the database's clock (or equal to the event before, while a clock stepped back catches up). The service takes that time from the database's clock; the database checks only that it falls in that window.
- The proposal, approval and execution events carry a digest of the payload, and a delivery event a digest of the exact body sent, so a payload rewritten in the table later no longer matches the log. The payload as the agent proposed it is kept beside any edit, set by the database and never changed.
- People, agents, mandates and actions are changed only by an owner. Revoking an agent, a token or a mandate and removing a person are final, and the last owner cannot be removed, even when two owners remove each other at once. A removed person can be added again as a new person, and a removed action can be defined again.
- One tenant never sees another's rows, and the triggers filter by tenant themselves rather than relying on row level security alone.

**Only in the service.** The database does not check these.

- That an approval runs at the action's current risk when it is higher than the proposal's. The database lets an approval raise the risk; it does not require it.
- That a reused idempotency key with another action or payload is refused, and that the hash kept beside a key is the hash of that request. The database keeps one proposal per agent and key and keeps both columns unchanged; the comparison is made by the service.
- The payload against the action's schema, when it is proposed, edited, approved and allowed by a mandate. A schema changed after the decision does not reach back to a delivery already recorded.
- The steering-text tripwire and the flag it sets.
- The risk assessed from a payload by an embedded host's own code.
- The explicit confirmation before approving a high risk proposal.
- That an action which exists only as code in an embedded host may be delegated at all.

## What it does not protect against

- **An agent that still has its own credentials.** pgmandate governs what goes through it. Removing the direct access is the precondition, and it is outside this software.
- **Arbitrary SQL as the application role.** The database trusts the scope a transaction declares: tenant, actor, and the proposal being executed. Code that sets it honestly is held to the rules. An attacker who can run their own SQL as `mandate_app`, through an injection or a compromised server, can declare any scope and is not held. The guarantee is about bugs, not about a compromised process.
- **The database owner.** The owner bypasses row level security, can drop triggers, and can rebuild the log. Run `serve` with `MANDATE_APP_DATABASE_URL` so the server never holds the owner's credentials, and keep the log's latest hash somewhere else.
- **Reads.** What an agent reads, and where it sends it, is not seen.
- **An agent that can act as a person.** Locally, dev login lets anyone on the machine sign in as any email, the command line's `-as EMAIL` acts as any person with the owner's database connection, and the default passwords are in this repository. An agent with a shell or network access on the same machine can approve its own proposals. The local setup is for trying things out; run the agent as another user or in a container, and run pgmandate as described under "Running it" before an agent depends on it.
- **Whoever holds a connector's secret.** It is held by pgmandate and by the receiver. Either can sign a body the receiver accepts, with no proposal behind it. The secret changes when the action's URL changes, and the deliveries still waiting move to the new URL, so a receiver the action moved away from gets nothing more, apart from an attempt already in flight at that moment. Rotate it by hand when a receiver is retired some other way.
- **Any session of the application role declaring itself the system.** The role is one; the scope a transaction declares is trusted. Code that declares the system scope wrongly can read connector secrets through `connector_of` and execute under a mandate. A separate role for the part that sends is on the roadmap.
- **A careless approver.** A person who approves everything is not oversight. Many approvals in a minute are written to the log; nothing stops them.
- **Injection in general.** The tripwire catches a few English phrasings. A paraphrase, another language or an encoding passes.
- **A malicious owner of a tenant.** An owner can grant anything up to medium risk and point an action at any public address. Connectors on local and private addresses, and plain http, are refused unless the operator allows them, and redirects are never followed, so the server cannot be made to call into its own network or send a decision across the internet unencrypted.
- **One tenant slowing another.** Deliveries have their own job queue, so a slow connector does not hold up decisions. But all tenants share that queue and its workers: one owner who points actions at a connector that never answers delays every tenant's deliveries.

## Known gaps

- Connector signing secrets are stored unencrypted, because the server signs with them. The application role cannot read them except as the system, but the database owner, a backup or a copy of the disk can.
- A payload cannot be erased. Proposals are append-only, and only removing the whole tenant deletes them, so a request to erase personal data in one proposal cannot be met.
- A person is identified by the verified email address from the sign-in provider, not by the provider's subject id. The provider must serve a single organisation.
- Sessions last a week by default and are not tied to the provider's session. A session is by email and spans tenants: removing a person from a tenant ends their access to it on the next call, and leaves their other tenants alone. Disabling them only at the provider does not end a session until it runs out.
- There is no rate limit on failed sign-ins or bad tokens. The limit on an agent's calls is kept in each server's memory, so several instances each allow the full rate.
- A pending proposal never expires, and an agent cannot withdraw one.
- An agent that sends the same request under a new idempotency key has made a second proposal. Nothing compares the content of proposals.
- What a person does through the API is not idempotent: a grant sent twice is two mandates, each with its own budget. The web UI disables the button while a call runs; the API has no key for it.
- If the final failure of a delivery cannot be recorded, the row stays pending; nothing reconciles deliveries against the job queue.
- A connector on an address that may not be called is accepted when the action is defined and refused when the first delivery is attempted.
- The reason a proposal waits is written to the log when a mandate was tried and did not allow it. For a proposal that could never be delegated (flagged, high risk, or an action that is not delegable) the reason is worked out when it is shown, not recorded.
- Lists return the newest few hundred rows, and exports are built in memory. Neither will hold for a large tenant.
- The log is one lock per tenant. Every agent call writes to it, so a busy tenant serialises on it.

## Not reviewed

There has been no external security review. The service was built with an AI coding agent under its author's direction, and reviewed by its author and by several rounds of AI reviewers taking different roles (staff engineer, security, agent developer, interface, positioning, first-time user). No independent person has tried to break it. Treat it accordingly.
