# Decisions

Newest first. A decision that changes the rules also changes `AGENTS.md`. A decision that changes what pgmandate is also changes [scope.md](scope.md).

## 2026-11-25: one image and a ten-minute quickstart

Built as [design-quickstart.md](design-quickstart.md) says, with its questions settled there.

- **The image** is published to `ghcr.io/andreas-lindfalk/pgmandate` by the `release` workflow when a `v*` tag is pushed, for amd64 and arm64, with the tag as its version. It has a `/data` the server may write, owned by the user it runs as.
- **Sign-in links** (`sign_in_links`, hashed, single-use, at most an hour by the table's own check, lifetimes on the database's clock): a setup link the server prints while there is no tenant, which makes the first tenant and its owner and signs them in, in one transaction with the link; and a person's link from `pgmandate sign-in-link`. Both sessions are pinned to their person (`sessions.person_id`). Every link that does not work is refused alike. The web app takes the token from the fragment and signs in by an HttpOnly cookie.
- **`MANDATE_SIGNING_KEY_CREATE`** makes the key file once, only when asked: written whole to a temporary file and linked into place, so a crash leaves no half key and two servers keep one. An empty key file is an error that names it.
- **Found in review and closed:** one setup at a time across servers (an advisory lock taken by making a setup link and by using one), so two links never make two tenants; a server that starts while a tenant is being made starts anyway; the database refuses a setup link once a tenant exists, at use as at insert; expired links are deleted with expired sessions, and every link goes with its tenant when the owner purges it.
- **The links table is global, like sessions,** without row level security: a link is found by its hash before anyone is in scope. The app role can read it as it reads sessions; the threat model says so.
- **`deploy/quickstart/`**: compose with Postgres and the image, nothing to set by hand, and `smoke.sh`, which the `quickstart` workflow runs on each pull request that touches the server, the web app or `deploy/`, and on `main`. It was run locally: setup, an agent's proposal approved, and a sign-in link redeemed.

## 2026-11-25: sign-in that holds up

Built as [design-sign-in.md](design-sign-in.md) says; its three questions were answered as proposed.

- **A person is bound to one identity per tenant,** at their first call in it: `persons.issuer` and `subject`, set by the system once, both together, never changed, unique per tenant, with `person.bound` in the log (the issuer, and the subject as a digest, as the log keeps no email). Sessions keep the identity from the verified ID token.
- **Dev login stays by email** and binds nobody.
- **No unbind:** an owner removes and re-adds the person.
- **Failed attempts** (`pkg/authlimit`): an HTTP middleware in front of everything that checks credentials counts a 401 to a request that carried a token or the session cookie, and a failed sign-in callback that carried the sign-in's own cookie; twenty a minute per client address (an IPv6 address by its /64), then 429 for every credential check from it until the minute ends. A request with neither is no failure: a signed-out page does not lock anyone out, and neither does a page elsewhere that makes browsers fetch the callback. A host that embeds pgmandate puts the same middleware in front of its own mux.
- **Found in review and closed:** gRPC and gRPC-Web are refused where credentials are checked, since they answer a refusal with HTTP 200 and would go uncounted; pgmandate's clients speak Connect. A session cookie that opens nothing is cleared in the answer, so a page left open after its session ends stops failing. The client address header is read from its last line, which a proxy that adds a line writes. The map of addresses is bounded and swept at most once a minute. The sign-in lookup uses an index for the email and one for the identity, rather than scanning every person.

## 2026-11-25: setup without a superuser

- **Setup sets no attribute only a superuser may set.** `Bootstrap` creates `mandate_app` with `LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS`, which an admin with `CREATEROLE` may give. An existing role's password is set only when `MANDATE_APP_PASSWORD` is given, never to the local default; from Postgres 16 on only the role's maker may, and a refusal says so. It no longer corrects an existing role's attributes, which needed a superuser; it checks them and refuses.
- **The check** (`db.CheckAppRole`) refuses an app role that cannot log in, may set `session_replication_role` (which turns triggers off), or is, or can become through membership with or without `INHERIT` (since `SET ROLE` is enough), a role that is a superuser, bypasses row level security, creates roles, replicates (and so reads every change through logical decoding), is a managed service's admin (a list of known names), is one of `pg_execute_server_program`, `pg_read_server_files`, `pg_write_server_files`, `pg_read_all_data` or `pg_write_all_data`, or owns pgmandate's schemas, tables or functions. `migrate` runs it before and after the migrations; `serve` runs it as the app role when it starts. A role that may create in `public` is warned about, not refused, since a shared database may need the grant.
- **A role a DBA made** is used as it is with `MANDATE_CREATE_APP_ROLE=false`; its password changes only when one is given.
- **`REVOKE CREATE ON SCHEMA public FROM PUBLIC`** runs only with `MANDATE_REVOKE_PUBLIC_CREATE=true`. Postgres 15 and later grant none by default, and on a shared or managed database the schema may not be ours to change.
- **Tests:** a bare Postgres where an admin with `CREATEROLE` and no superuser sets everything up, a role made beforehand, each kind of role that is too strong, and the command line end to end. They run in CI with the rest, so setup as a non-superuser is held there (the first part of roadmap item 11).
- **Not checked:** `CREATEDB`, which does not get past the rules, and a role changed while the server runs, which its next start catches.

## 2026-11-25: one version, and diagrams GitHub draws as intended

- **One version** (`pkg/version`): what a release build sets with `-ldflags -X`, otherwise what Go stamps from git (a tag or a pseudo-version, `+dirty` for uncommitted changes), otherwise `dev`. `pgmandate version`, the server's start-up log, the MCP server and the command line's own MCP client report it. The client now calls itself `pgmandate-cli`, the name that reaches the log as provenance. The image takes it as the `VERSION` build argument, the release's tag, since `.git` is not in the build context; compose passes `MANDATE_VERSION`, and the build refuses a value the log could not keep as provenance. A tagged release reports the same string either way; between releases a local build reports Go's pseudo-version and an image `dev`. `-v` is not an alias, so that it stays free for verbosity.
- **Diagrams.** GitHub clipped the labels of the Mermaid diagrams and drew the agent on the far right. They now use SVG labels (`htmlLabels: false`), which are measured in the font they are drawn in, and one two-way edge per pair instead of a separate edge back, so every node keeps its place from left to right.

## 2026-11-25: build step 4, the dry run

Built as [design-0.1.md](design-0.1.md) section 1 sets out.

- **A new agent starts in dry run** (`agents.mode`, default `dry_run`). An owner creates one live (`-mode live`, or *Live from the start* in the UI) or switches it (`SetAgentMode`, `pgmandate agent mode`), logged as `agent.mode_changed`. The fixture of the tests, the demo's main agent and the quickstart create agents live, and say so.
- **The database holds it.** The insert trigger sets `proposals.dry_run` from the agent's mode, whatever the insert says, and the update trigger never lets it change. Two table checks and the update trigger keep a dry run from becoming approved, rejected or executed, and a live proposal from becoming *would allow* or *dismissed*. *Would allow* is held to the same checks as an execution (the mandate locked, `basis_holds`); only `executed` counts against a budget. `may_write_for` opens a host's table only for a live proposal it can see, so it fails closed, and the deliveries guard refuses a delivery for a dry run. `may_write_for` moved below the proposals table it now reads, and stays plain SQL.
- **The execution job runs a dry run as far as the decision**, records *would allow* with the basis, and stops: nothing is applied, nothing delivered.
- **The person's side.** `DismissProposal` and `pgmandate dismiss`; approving or rejecting a dry run is `failed_precondition`. A waiting dry run sends no chat notice.
- **The agent's side.** Every answer of the agent contract carries `dryRun`, false included. Only the answer that made a dry run tells the agent to do it itself, once; a repeat or a later read says not to do it again, so a framework that resends cannot make the agent act twice. `get_proposal` does not wait for a dry run, since nobody decides it.
- **The inbox keeps dry runs apart.** Waiting dry runs have their own tab and are not in the *Waiting* queue or its count (`dry_run` filter on `ListProposals`), so they cannot crowd out what a person must decide.
- **The record.** Every event of a dry run carries `dryRun: true`; the evidence page marks them. The decisions export already took only approved, rejected and executed, so dry runs stay out. There are no agreement figures yet to keep them out of.
- **The demo** gains a step: a second agent in dry run, a *would allow* with nothing delivered or spent, and a dismissal.

## 2026-11-25: build step 3, the agent contract

Built as [design-0.1.md](design-0.1.md) section 5 sets out.

- **One service, two doors.** `modules/agentapi` implements `mandate.agent.v1` as a Connect handler. `mcp/` calls it in process, so the rate limit per token (shared by both doors), the waits, the guidance texts and the record of each call in the log exist once. The log records the door, `via: mcp` or `via: http`.
- **The answer is the contract's message.** Over MCP, each answer is the message's JSON with every `*Json` text field turned into the object it holds, under the name without `Json` (`mcp.Structured`). So `propose` answers `{"created", "repeat", "accountIgnored", "proposal": {…}}` rather than one flat object, and a test holds the two doors to the same answer.
- **Zero is said where it means something.** `created`, a mandate's `remaining`, a budget's `spent` and a delivery's `attempts` are `optional`, so they are in the JSON at zero; other empty fields and lists are left out, as protojson does. Counts are `int32`, so they are JSON numbers, not strings; only an event's `seq` stays `int64`.
- **A caller that leaves is still recorded.** The call goes into the log with a context that is not cancelled, and the caller is told `canceled` or `deadline_exceeded` (MCP: `canceled`), which, like `internal`, may have stored a proposal.
- **The fix the review found:** the kernel's execution job read `Conflict` as "decided meanwhile". With the new code it reads `FailedPrecondition`, so a proposal decided or expired under it gets no false `execution_skipped` and no notification.
- **Provenance over HTTP** is what the agent sends; over MCP the client's own introduction replaces it.
- **A new code, `FailedPrecondition`,** for a change to a proposal that no longer waits: decided, withdrawn or expired. It replaces `Conflict` there for people too (`failed_precondition` in the people's API, where `already_exists` was). MCP keeps its readable name, `conflict`, for both that and a reused key.
- **Found on the way: a grant could start in the database's future.** A mandate's `valid_from` was the server's clock, and `basis_holds` checks it against the database's. With the database's clock behind, an execution right after a grant was refused; on Docker for Mac this made `Test_ApplyFailureLeavesItPending` fail now and then, on `main` too. A grant now starts at the earlier of the two clocks, with its length kept. The rest of the mandates' reads still use the server's clock, which tests fake; moving them onto the database's clock is a change of its own.

## 2026-11-25: build step 2, delivery contract v2

Built as [design-0.1.md](design-0.1.md) sections 3 and 4 set out. v1 and its shared secrets are gone, with nothing kept compatible.

- **The signing key lives outside the database.** `MANDATE_SIGNING_KEY` or `MANDATE_SIGNING_KEY_FILE`, or, on loopback only, one made once in `.local/signing-key`. Beyond loopback a missing key stops the server. `MANDATE_SIGNING_KEYS_PREVIOUS` are published at `/.well-known/pgmandate-keys` and never sign. The compose stack requires the key.
- **The audience replaces the connector secret.** The actions guard draws it on insert, on a move, on a switch back to webhook, and on redefinition after removal, and ignores whatever a caller writes. `connector_of` is gone: the address and audience are not secret, and the application role reads them like the rest of the row.
- **The database holds the delivery's states.** Seven states and every transition in the design note's table, in `deliveries_guard`, with tests that bypass the Go code. An attempt is opened in its own committed transaction before the call, and proven unsent only when the request's headers were never written.
- **A late answer is a conflict.** An explicit answer that arrives after its attempt was closed is logged as `delivery.conflict` and changes nothing.
- **The sweep finishes deliveries** past `not_after − 15 s` with no attempt open, and closes attempts left open longer than 45 seconds, which stay counted as possibly sent. A host without a queue calls `Kernel.FinishDue`.
- **The person's side:** `SettleDelivery` in the API and `pgmandate settle` on the command line; `RedeliverProposal` and `redeliver` send again only a delivery never sent, before its deadline. The inbox shows each state, offers "send again" only while it is allowed and "settle" only once it is.
- **The agent is told what to do per state.** An unknown outcome tells it not to propose again; a refusal, a delivery settled as not done, or one never sent past its deadline tell it that it may, under a new key.
- **The reference receivers** (`examples/webhook`, `examples/refunds`, `pgmandate receiver`) follow the receiver's order in the note, including 503 for a delivery still in progress and `too_late` before acting.
- **Rotation is by restart.** The new key signs at once. A receiver that meets a key id it does not know fetches the published set again, at most once a minute (`sign.Fetch`, and `Refresh` in `examples/webhook`); until it has, it answers 401 and the attempt is retried. Publishing a next key before it signs was not needed for 0.1.0.
- **A body for an audience the receiver does not know is retried, not refused.** A delivery that moves with its action carries the new audience before anyone can have told the receiver at the new address, so a final refusal would lose approved work over a configuration step. The reference receiver answers 403 without an outcome.

## 2026-11-25: build step 1, the fresh schema and expiry

- **One fresh migration.** 0001 to 0008 are folded into one `0001_init`. Before the step's own changes, the fold was checked against the old chain: the two schema dumps differ only in the order of two equivalent check constraints' generated names. A local database from before is reset with `make reset`.
- **Budgets count executions.** `budget_units` is `budget_count`, and actions and proposals no longer carry a cost. The basis cites `budgets: [{kind: "count", limit, spent}]`, and `basis_holds` accepts exactly that one budget, with the spend the database counts and room for one more. Delivery contract v1 carries the same list until v2 replaces it.
- **Proposal expiry**, as the design note says: a deadline and a delivery window fixed on each proposal from its action, a trigger that allows nothing but the system's expiry from the deadline on, and a sweep every minute on River's leader. A host without a queue calls `Kernel.ExpireDue`.
- **River in its own schema**, `mandate_river`. The application role is given nothing of River's in `public`.
- **What "the action changed" means now:** a payload that no longer fits the schema, or a webhook action's raised risk.
- **Found on the way:** an execution can take up to about a second to start, because River fetches on an interval when it is not woken. `main` behaves the same. The agent-facing tests that wait two seconds for an execution can therefore fail on a loaded machine. That is worth a look of its own; this step does not change it.

## 2026-11-25: the design of what 0.1.0 freezes

[design-0.1.md](design-0.1.md) designs proposal expiry, the dry run, delivery contract v2 and the agent contract together. Two reviews (protocol and security, and fit with the code) shaped it. The choices that matter:

- **Only an explicit answer from a receiver is final.** `done`, `refused` and `unknown` must be said. A proxy's 404, a redirect or a timeout is never read as "nothing was done".
- **The worker writes ahead.** It records that an attempt started before calling. A crash then ends as *unknown*, never *not sent*.
- **Retries stop before `notAfter`.** An unknown delivery is settled by a person only once the work can no longer happen. A delivery is sent again only from *not sent*.
- **An audience has a random part,** drawn again on every definition or move, so it never repeats.
- **A result reported later is left out of 0.1.0.** It brought a token that is hard to keep both stable and secret, an extra endpoint and races. It is additive, so it can come later.
- **One periodic sweep on River's leader** expires proposals and finishes deliveries, tenant by tenant. An embedded host without a queue calls it itself.
- **Payloads in the agent contract are JSON text,** so numbers keep every digit.
- **A new agent starts in dry run.** It changes nothing until an owner makes it live, and an owner may create it live. A waiting dry run can only be dismissed, never marked as "would approve".

## 2026-11-25: the roadmap follows a first team

Three critical readings of the scope and the roadmap each came back "go, with changes". These are the changes. They revise the roadmap order in the entry below.

- **A first team, not more features, is the critical path.** Finding one is a track of its own from the start, with a checkpoint when phase 1 is done. Phase 1 makes pgmandate easy to try, safe to pilot, and fixes its contracts. Phase 2 becomes a list to choose from, ordered by what the first team runs into.
- **Pulled into phase 1:**
  - expiry of waiting proposals
  - a dry run as the way in, since no team removes an agent's credentials on day one. A dry run delivers nothing, since a receiver that ignores an unknown field would otherwise carry it out. It is recorded on the proposal by the database, kept out of budgets and the decisions export (the log keeps it, tagged), and named in the scope as the one exception to its first principle
  - people identified by subject id, and limits on failed sign-ins and bad tokens
- **Contracts before anything is built on them.** Delivery contract v2 comes before any receiver template, and must hold phase 2 without a v3. It carries:
  - signing with Ed25519, with the key outside the database, a key id and an audience. Key-pair signing has to change receivers' code once, so it happens before there are receivers.
  - a budget block with a kind, so that money is an addition
  - `notAfter`, so an expired decision is not carried out late
  - the *outcome unknown* status
  - a result reported later, authenticated by a single-use token per delivery (revised 2026-11-25: left out of 0.1.0, see above)

  The agent's operations become one Connect contract that MCP, HTTP and the clients share.
- **River in a schema of its own.** Queue names alone would not separate pgmandate's jobs from a host's, and its tables live in the host's `public` schema today.
- **A framework adapter must govern, not just approve.** Answering a framework's approval hook pauses a tool that still runs with the agent's credential. That is recorded approval. A governed tool's body proposes and waits for the receiver's result. The scope says so.
- **Budgets:**
  - **Phase 1:** a budget counts executions; "units" and an action's fixed cost go.
  - **Phase 2:** money, as a second limit beside the count, an amount per currency with no exchange between them. The database reads the cost from the payload itself, so the second gate holds for budgets too, and the cost field must be the one the receiver carries out.
  - Money does not stop an agent splitting a request; the scope now says it caps the splitting at the month's budget instead of closing the gap.
- **The MCP bridge waits for a team that needs it.** It depends on outcome unknown and upstream OAuth. It proposes mappings and an owner confirms them, so the bridge cannot raise its own authority. It reports what it called, so the log shows it.
- **No compatibility before 0.1.0.** Nobody runs pgmandate, so until 0.1.0:
  - the schema, the migrations and the contracts change freely
  - migrations may be folded into one fresh first migration
  - a 0.0.1 database is reset, not upgraded

  The rule that a migration on `main` is never changed applies from 0.1.0 on. A pilot starts on 0.1.0. This replaces the "only new files" rule of 2026-11-24 for the time before 0.1.0, and makes the migration 0008 approach optional rather than required.
- **A design note before code.** The roadmap says what each item must achieve and must not break. Before an item is built, a short note in `docs/` gives its states, migration, contract changes and tests, and is reviewed like code. Expiry, the dry run, contract v2 and the agent contract are designed together, since contracts and migrations cannot be changed once they ship. The roadmap carries the constraints the readings found:
  - a dry run never becomes *executed*, and the database refuses to deliver it, count it as spend, or open a host's table for it
  - an audience assigned per tenant and connector
  - budgets as a list
  - from 0.1.0 on, rows that exist at an upgrade get a stated treatment, and data changes that write to the log run as a Go step, never in SQL
  - a result deadline (`resultBy`) for work a receiver finishes later, so "not done" never comes while the work can still happen, and a late-result token stored only as a hash
  - identity as issuer and subject together
  - one outcome per delivery
  - River moved without losing a job
- **Scope for a pilot.** The scope says what a first team does with 0.0.1, how a Python agent uses it today, what changes in 0.1.0, and plainly that personal data in a payload cannot be erased yet. The comparison with other tools moves to [landscape.md](landscape.md).
- **Durable execution and evaluation named.** Where a team runs Temporal or Restate, the adapter's natural form is an activity that proposes, with the decision arriving as a signal. The decisions export is a labelled set for evaluation tools.
- **A correction to the threat model.** It listed the log's digests as enforced by the database. The database checks that each event continues the chain and carries its session's actor. The hash, the digests, and whether a decision writes an event at all are the service's, and are now listed so.
- **Moved to later:**
  - embedding beyond the River and setup fixes, which phase 1 keeps so that "embeddable" is true
  - more notification channels and routing, except a plain webhook notifier, which stays in phase 2
  - typed tools
  - the regulatory mapping

  Phase 1 turns the assumptions under *What stays flexible* into CI tests, so they are checked rather than stated.
- **The callback is owner-registered,** signed under an audience of its own so it can never pass for a delivery, and says only that a proposal changed. An agent never chooses where pgmandate calls.

## 2026-11-25: the scope, written down

Its roadmap order was revised the same day; see the entry above.

- **The aim is to be useful to other teams, not to be a product.** pgmandate stays an open-source project with no company behind it. Being useful means being tried, embedded and run by teams other than its author's. "Reference implementation" is no longer how it describes itself: it is meant to be used, and its status says how far along it is.
- **[scope.md](scope.md) is the canonical description.** It covers what pgmandate is and is not, the terms, where it fits, what it brings and who it is for. The README, AGENTS.md, the other documents, the UI and the help texts follow it. The contradictions found when it was written are fixed in the same change:
  - the number of MCP tools
  - whether an agent can withdraw
  - "no code path around this package"
  - "adopted policy document"
  - task and run
  - the old name in prose
- **Positioning: between agents and the systems they change, next to gateways and frameworks.** pgmandate does not compete with gateways, identity providers, policy engines or the approval steps in agent frameworks; it works with them. It leads with what is particular to it:
  - the mandate as a bounded permission a named person grants
  - Postgres as the second gate
  - approval bound to what runs, with a receiver that checks the claims it is given

  It does not lead with the hash-chained log or with asking a person, which are common by now.
- **The tagline says what the database does.**
  - "Postgres is the last gate" was the thesis behind the name, and it holds for the decision. But the receiver acts after Postgres, so the tagline overstated it. It now reads: *Agents propose. A person, or a standing permission a person granted, decides. Postgres enforces the rules a second time.*
  - "The database decides" leaves the UI and the command line, since a person or a mandate decides.
  - The scope also states the limits of the principles: which rules only the service holds, that the log is tamper-evident only to someone who keeps its latest hash elsewhere, and that the database guards against bugs in the service, not against a compromised one.
- **Every deployment shape stays open.** The shapes are its own service (the main one), embedded in a Go service, behind a gateway or framework, and hosted for several organisations. Hosted is not built, but nothing may close it. Credentials to customers' systems live with receivers, so a hosted pgmandate would hold none of them. It would still hold the secrets receivers trust until deliveries are signed with a key pair (revised above: key-pair signing is now part of delivery contract v2, in phase 1).
- **Postgres is required, in any language.** The database as the second gate is the point, so it is not pluggable. That pgmandate is written in Go matters only to a host that embeds it.
- **It comes with a UI, and the API is the contract.** The web UI uses only the people API, so a team can build its own UI or wire an existing admin tool to the same API.
- **One repository.** Clients, framework adapters, receiver templates and the MCP bridge live here, under their own directories.
- **Budgets in money or as a count, not units.** A person granting a mandate must understand what they grant. An amount of money, as with prepaid API credit, or a number of executions does that; "units" does not. An action's cost becomes a fixed amount or a payload field with its currency. A cost that cannot be read waits for a person. Planned, not built. (Revised above: a count of executions in phase 1, and money from the payload as a second limit in phase 2.)
- **Reversed: pgmandate will not sit in front of MCP servers.** It gets a bridge instead: a receiver, run next to the system, that calls an existing MCP server's tools once a proposal is decided. The goal of the earlier decision stays, an existing MCP server governed in ten minutes with no code, and so do its design points (mapping, a hash of each tool's definition, at most once with an outcome that can be unknown, no reads). Why:
  - The upstream credential stays next to the system instead of in pgmandate's database, for every tenant.
  - pgmandate stays out of the gateway market, where others are ahead.
  - The groundwork the front needed first is no longer a precondition: encrypted upstream credentials, upstream OAuth, and a separate sending role. The sending role stays a known gap in the threat model and is listed under later.

  What the bridge gives up: it has no records of its own and cannot check the agent's claims. Bridged actions therefore default to a person deciding. The roadmap carries the rest: at most once with an outcome that can be unknown, least privilege upstream, and untrusted results.
- **The append-only guard is tightened (migration 0008).** A security reading of the scope found that the guard let a delete through whenever the session setting `mandate.allow_purge` was on, a setting any role can set. Nothing was exposed, since the application role holds no delete right on those tables, but a grant added later would have opened them. Now the statement must also run as the table's owner, as `purge_tenant` does. The fix is a new migration rather than an edit to 0001, so databases that already ran 0001 get it too.
- **The roadmap is rewritten in that light.** Phase 1 makes pgmandate tryable and embeddable. Phase 2 meets agents and systems where they are: HTTP, Python and TypeScript, framework adapters, receiver templates, money budgets and the bridge. The rest waits until someone needs it. (Revised above: the HTTP agent API is now phase 1, through the agent contract, and phase 2 follows a first team.)

## 2026-11-25: conditions, the part the database can check

- **Built now, ahead of the order below**, on a branch: a mandate can carry a condition on the payload, such as `payload.amount <= 500 && payload.currency in ["EUR", "SEK"]`. The MCP front and its groundwork keep their place after it.
- **One language, from the start.** Conditions are written in CEL, but only the part the database can check is accepted: payload fields and the action compared with literal values, `in` a list, `&&`, `||`, `!` and `has()`. Anything else is refused at the grant with what and where. The person granting writes one condition; there is no second form to keep in step.
- **One plan, two evaluators.** The service compiles the condition to a plan at the grant and stores both. The service and the database evaluate the same plan, and a test runs both over the same cases, malformed plans included. The execution's basis cites the condition; the database checks that it is the mandate's own and that the plan holds for the payload executed.
- **Exact, and stricter than CEL.** Numbers compare as decimals, not doubles: `500` equals `500.0`, and `0.1` is `0.1`. Strings compare by their bytes. A missing field, a field of another type than the literal (`"500"` against `500`) and anything else that cannot be evaluated is unknown, and unknown never allows; plain CEL would call `"5" != 5` true.
- **The grant catches mistakes it can see**: a payload field no covered action's schema declares, a literal of a type the schema never gives the field, and an action the condition requires that the mandate does not cover. When a covered action has no schema, the fields cannot be checked and are not.
- **Rolling back the migration is refused** while a mandate with a condition still holds: without its condition it would allow more than was granted.
- **Not yet:** a cost taken from the payload, a refused outcome, and the rest of CEL for what comparisons cannot say.

## 2026-11-25: a review round, with a pilot in mind

Six fresh reviews (security, Postgres, the agent's contract, the person's interface, a pilot's fitness, the market) found these, all fixed with tests:

- **A key a receiver could read as another is refused.** A payload with `amountCents` and `amountcents` passed a condition on one and could be carried out on the other by a decoder that ignores case. The service and the database refuse such payloads.
- **A condition plan is checked by the database**, not only produced by the service: known operators, paths and literal kinds, bounded depth and size. `in` over values that do not compare is unknown, not false.
- **Counts are made under read committed only.** The budget and the agent's limits rely on a lock that a repeatable-read snapshot sees past; such transactions are refused. The limit's count uses its index.
- **A receiver's refusal is kept and shown**, to the person and to the agent, which is told not to resend it unchanged. The reference receiver tells a refusal (422) from a failure (500) and remembers only what it did.
- **An agent can withdraw its own waiting proposal**, so a correction never leaves two versions for a person to approve.
- **The agent is told more and asked for less**: what a condition missed, its limits, that a repeat kept the first account, never to bend facts or split an action to meet a condition, and to keep personal data and credentials out of what it writes. Labels with spaces are made labels rather than refused.
- **The inbox shows what will happen first**, then what pgmandate checked, then the agent's account folded and marked as its claim; flagged text is hidden until asked for. A link from a chat notification survives sign-in. Notifications can be tested.
- **The comparison is brought up to date** with AWS AgentCore Policy, Microsoft's Agent Governance Toolkit, LangSmith's Tool Gateway and DigitalOcean's Action Gateway, all of which decide on a call's arguments too.

## 2026-11-25: what a first pilot needs

- **Every proposal carries the agent's account.** A person cannot decide well on a title and a payload, and a record without the agent's reasoning says little about what agents are up to. The rationale is required, like the idempotency key; evidence (a source, a reference to find it again, a summary) and provenance (the client, the model, the agent's version) are optional. They are kept as written and never edited, the log holds their digests and the provenance labels, and they are not part of the request a key names, so a retry in other words is the same request. A mandate never reads them.
- **Why a proposal waits is spelled out.** When a condition stopped it, the inbox names the comparison that failed and what the payload had instead. Conditions can be written in the grant dialog.
- **A chat channel is told when something waits**, through a Slack or Discord incoming webhook an owner sets, one per tenant. The message carries nothing the agent wrote, so a proposal cannot post into the channel or put personal data into the chat tool. Its address is a secret, kept like a connector's. Approving from the chat is not built: the link opens the inbox.
- **Limits per agent**, 30 proposals a minute and 300 an hour by default, held by a trigger so they hold across instances. This was phase 0 item 5 of the roadmap.
- **A receiver checks the claims it is given.** A condition decides on what the agent wrote; a receiver of an action that moves value reads each fact from its own records and refuses a mismatch. `examples/refunds` shows it, and the reference receiver can now carry out a delivery and refuse it with a 422.

## 2026-11-25: the name, and what goes before the first push

- **Renamed to pgmandate.** "Mandate" became the common word for agent authority this year (an IETF draft on agent mandates, Google's payment mandates, several projects), and a search for the project found everything else. pgmandate keeps the concept and names the thesis: Postgres is the last gate. A *mandate* stays the word for a standing grant.
- **A 401 is retried.** After a secret is rotated, the receiver answers 401 until it has the new one; the delivery should survive that. A 403 stays final: it means "never for me".
- **Connector secrets are not readable by the application.** The app role has no right to the column; only a transaction scoped as the system gets a secret, together with the address it belongs to, and sends there. A delivery found behind a moved action is moved before it is sent.
- **Agents get what they need to repeat safely:** a suggested run id when their host gave none, a repeat that says it is one and keeps the original guidance, structured answers and error codes, and a `propose` command for trying it without an agent.
- **Groundwork before the MCP front is only what the front needs**: encrypted credentials, a separate role for the part that sends, upstream calls at most once, expiry with a callback, per-agent limits, and how pgmandate authenticates upstream. The rest waits.

## 2026-11-25: groundwork first, then the MCP front, then conditions

- **Order changed.** Taking adoption cost out comes before expressive rules: a first outside user is blocked by having to write a receiver, not by missing conditions. This replaces the order in "conditions next" below.
- **Conditions start where the database can check them**: comparisons on payload fields and a cost from a payload field, compiled into the check of a mandate's basis. CEL comes after, for what those cannot say, and is a rule the service holds.
- **Signed mandates an agent carries are dropped.** They would give the agent a credential again and cannot be counted against a budget offline.
- **The pitch is narrower.** The README leads with a reference implementation of write authority with Postgres as the last gate, and names the tools that do much of the same. "One record of every agent action" moves to "What a product would need".
- **More rules in the database:** a delivery only in the transaction that decides; no execution when the action's cost or risk changed since the proposal; times in the log from the database's clock, never going back; triggers that filter by tenant themselves.
- **The log names what was decided.** Digests of the payload proposed, approved and executed, of the body delivered and where it went, of the connector an approval was made against, and of an action's URL and schema. The proposed payload is kept beside an edit.
- **Smaller corrections:** a payload over MCP keeps every digit; a moved connector gets a new secret and takes its waiting deliveries along; removing a person from one tenant no longer signs them out of the others; the command line's default cost is one unit; a high risk webhook action cannot be marked delegable; deliveries have their own job queue; the deploy example runs the server with the application role only and refuses the default password beyond loopback.

## 2026-11-25: in front of existing MCP servers

Its order and its choke-point list were revised the same day; see the entry above.

- **Reversed:** pgmandate will sit in front of an existing MCP server. The roadmap said this was for the gateways. A receiver written for every protected system will not be adopted, and a single record of every agent action is only worth having if bringing a system in costs almost nothing.
- **Still true:** the agent holds no credentials. In front of an MCP server, the credential for that server is pgmandate's, and a change is a proposal decided as any other.
- **The measure:** an existing MCP server governed in ten minutes, with no code.
- **A choke point is designed out, not accepted:** failing closed for changes, several instances, a latency budget, and signed mandates that let low risk actions continue when the service is down. The roadmap lists it.
- **The roadmap is in phases:** adoptable (conditions, the MCP front, an HTTP connector, decisions in chat tools, expiry), every kind of agent (HTTP clients, framework hooks, OpenTelemetry, two approvers), and the record paying for itself (inventory, suggested mandates, exports, an anchored log).

## 2026-11-24: conditions next

- **Mandates get conditions**, in CEL, over the payload and the context of the request, with cost from the payload and a refused outcome. Built on a branch after the first version is on `main`. The design is in the roadmap.
- **Why.** A mandate today sees the action, its risk, its cost and the budget, never the content. People should see what they need to and nothing else; that needs rules about the content.
- **What it costs.** The rule that let a proposal through is evaluated by the service, not the database. The database still checks which rule was cited.

## 2026-11-24: done once

- **Every proposal has an idempotency key**, required by the service and by the table, unique per agent. Agents repeat requests, and a layer in front of side effects has to make that safe rather than leave it to the caller's goodwill.
- **A repeat returns the first proposal**, whatever has happened to it or to the action since: it is looked up before anything is checked against the action as it is today. The same key with another action or payload is refused.
- **What was asked for is kept as a hash** beside the key. A person's edit changes the payload and not the hash, so a repeat after an edit is the same request and not a conflict.
- **No guessing from content.** Two proposals with the same content and different keys are two requests. Treating them as one would be a guess, and two identical orders can be meant.
- **One name per hop:** the key from agent to pgmandate, the proposal's id for the decision, the delivery id from pgmandate to the receiver.

## 2026-11-24: the delivery contract

- **A published body.** The body of a delivery is described by `docs/delivery-v1.schema.json`, and a test fails if the server sends a field the file does not name.
- **A version.** The body carries `version`. Fields may be added within a version; a removed field or a changed meaning is a new one. A receiver refuses a version it does not know.
- **A correlation id.** The agent's run id when it gave one, otherwise the proposal's id, so there is one value to join logs on and it is always there. `runId` stays beside it, so a receiver can tell which of the two it got. A run id is chosen by the agent: it groups, it does not identify.
- **The schema as it is at the decision.** A payload is checked when it is proposed, when it is edited, and again when it is approved or allowed by a mandate. A proposal made before the schema changed needs an edit that fits, or a rejection; a mandate leaves it for a person (`payload_invalid`).
- **Times.** When the proposal was made, when it was decided and when the delivery was recorded, the same on every attempt. The time of an attempt stays in the header, where the signature covers it.
- **A webhook action needs a payload schema**, in the service and in the table. An owner can still write a schema that accepts anything, and then that is a stated choice.
- **Still untyped:** the connector's answer. It is any JSON object of at most 4 kB.

## 2026-11-24: claims in line with the code, rules in the database

The first version was read from six seats (staff engineer, security, agent developer, interface design, positioning, a first-time user) before it went public. What I changed:

- **Claims brought in line with the code.** The README no longer says the log maps onto regulation, that no code path can do what the database refuses, or that steering text is detected. It says what is true: evidence that supports certain controls, protection against bugs in the service, and a tripwire. The threat model is its own document.
- **Rules moved into the database.** A proposal cannot understate the risk or cost of a defined action, a defined action that is not delegable is never allowed by a mandate, an edit exists only as part of an approval, a delivery goes only to its action's connector, and every log event carries its session's actor and must continue the chain.
- **People can be removed.** Removal is final, ends the person's sessions, and never applies to the last owner. The system creates only the first person of a tenant.
- **The agent learns what happened.** The connector's answer is kept as the result and returned. "Allowed" and "delivered" are told apart. A waiting proposal says what the agent should do, and `get_proposal` can wait.
- **Deliveries.** Connectors on local and private addresses are refused unless the operator allows them, redirects are not followed, errors are recorded without addresses, a 4xx is not retried, and a person can send a failed delivery again.
- **The server.** It can run with the application role only. It finishes running jobs on shutdown. With dev login on, it answers only to localhost.
- **Position.** The nearest neighbours are MCP gateways and governance proxies, which are easier to adopt. pgmandate is the smaller, stricter option: it removes the agent's access. The roadmap says so.
- **Scope.** A reference implementation and a demonstration, not a product. No hosted service.

## 2026-11-24: names and layout

- **Action**, not effect, for what an agent asks to have done: the word the field uses. A glossary in the README maps the rest.
- **Mandate** stays, although Google's Agent Payments Protocol uses the word for payment authorisations. The README says the two are unrelated.
- **Layout.** `modules/` for the domain and `pkg/` for shared plumbing. `kernel.go` and `sign/` stay at the root because outsiders import them. One migration set, since the triggers span all tables.

## 2026-11-24: a service, not a library

- **Standalone service first.** Local, hosted and on-premise from one image. Embedding the Go library stays possible for a host that shares the database.
- **Actions are data.** An owner defines an action with a connector: a signed webhook, or code in an embedded host.
- **Deliveries.** The decision is final in the database first; a job then calls the connector. The proposal's status never depends on the connector.
- **People sign in.** Sessions, dev login locally, OpenID Connect otherwise. A Connect API and a web UI.
- **Compliance is not the product.** The log is evidence that can support oversight and logging controls. No reports, no control library.
- **Tenant is the organisation.** Department is an optional label.

## 2026-11-24: the start

- **Thesis.** Agents propose; a person, or a person's standing permission, decides. Mandates are rows; the policy, the risk limit, the budget and the evidence live in Postgres, enforced by row level security and triggers as well as by the service.
- **Cornerstone.** *AI changes nothing that a person has not approved, either the proposal or the policy that allows it.*
- **Scope.** Small on purpose. Not an agent framework, not a gateway, not an orchestrator.
- **License.** Apache-2.0. Contributions are signed off under the Developer Certificate of Origin.
- **Language.** English everywhere: code, docs, commits.
- **Tenancy from the first day.** Every table with tenant data has `tenant_id` and a policy; the application role never bypasses row level security. Sessions are the exception: they are global, by email.
- **Risk.** Three levels. A mandate allows low or medium; high is never delegated.
- **Budget.** Per mandate, per UTC calendar month, in the tenant's own units. Counted from allowed proposals under that mandate, with the mandate rows locked, so two executions cannot both fit in the last unit.
- **Policy reference.** A mandate must name the policy document it rests on. pgmandate does not store the document; the reference is what the log and the basis carry.
- **Execution is a job**, inserted in the transaction that stores the proposal, so it exists if and only if the proposal does.
- **Agents only propose.** Every non-human proposer is an agent with a token. One actor kind, one rule.
- **The schema before the first push.** Until the schema has been pushed, the initial migration is edited in place. After that, every change is a new numbered migration, and a local database from before needs `make reset`.
