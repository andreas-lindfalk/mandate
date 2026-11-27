# Roadmap

What comes next, in order, and why. What pgmandate is and is not is in [scope.md](scope.md), and the roadmap stays inside it. Nothing here has a date.

0.1.0 is built and released, and 0.1.1 followed. A first pilot is being prepared, starting in a dry run. The order from here follows that pilot, and a review of the project and of the field in November 2026; [decisions.md](decisions.md) says what changed and why.

pgmandate has one maintainer, working evenings. What follows is an order and a menu, not a promise.

## The aim

pgmandate should be useful to teams other than its author's. Two milestones, in order:

1. **A first pilot goes live:** one agent's change to one real system goes through pgmandate, with the agent's credential to that system removed.
2. **The measure, as it was: one team outside its author runs an agent through it, with the agent's direct credentials removed.**

Both depend more on real use than on more features. So the work is in four parts:
- **[For a first pilot](#next-for-a-first-pilot):** what a team needs for a dry run, and then to go live. It is built in order.
- **[Evidence](#evidence-alongside):** tests and reviews that turn the project's claims into things anyone can check. It adds no features, and runs alongside.
- **[Being found](#being-found):** a write-up for each thing that ships.
- **[After the pilot](#after-the-pilot-a-list-to-choose-from):** a list to choose from. What gets built follows what the pilot runs into.

[Later](#later-when-someone-needs-it) and [Deliberately not](#deliberately-not) are lists kept so that nothing done earlier closes a door, or opens one that should stay shut.

One theme holds the first part together: **rules a person can trust before granting them.** A mandate lets an agent act without a person, so a person must be able to see what a rule would have done, and what bounds it, before signing it.

**Checkpoint after the pilot's dry run.** Its figures decide what is built next. If they show that rules are not worth granting, or if no outside team is in sight, the list after the pilot waits and this roadmap is reviewed.

**How an item is built.** The roadmap says what each item must achieve and what it must not break. Before any code, an item gets a short design note in `docs/`, reviewed like code. The note covers its states, its migration, its contract changes and its tests.

**Compatibility, from 0.1.0 on:**
- **Migrations.** A migration that has reached `main` is never changed.
- **Existing rows.** Rows that already exist at an upgrade get a stated, fixed treatment, never whatever a new column defaults to.
- **Data changes that write to the log** run as a step in Go after the schema migration. Such a step runs as the system, can be run again safely, and has a test that the chain still verifies after the upgrade. A SQL migration cannot write a correct log event.
- **Contracts.** The agent contract and delivery contract v2 gain fields; a change in meaning is a new version.

## Built: ready for a first team (0.1.0)

Where an item has a design note, it is linked; [decisions.md](decisions.md) has the reasoning.

| | Item | Note |
|---|---|---|
| 1 | One image and a ten-minute quickstart that needs only Docker | [design-quickstart.md](design-quickstart.md) |
| 2 | The README as a front door, with films of the UI and of MCP | |
| 3 | Database setup without a superuser, for managed Postgres | [running.md](running.md) |
| 4 | One version everywhere: the binary, the MCP server and the client | |
| 5 | Proposal expiry, with `notAfter` on deliveries | [design-0.1.md](design-0.1.md) |
| 6 | A dry run, as the way in | [design-0.1.md](design-0.1.md) |
| 7 | Sign-in by issuer and subject, bound per tenant | [design-sign-in.md](design-sign-in.md) |
| 8 | Delivery contract v2: Ed25519, an audience, a deadline, and only an explicit answer is final | [design-0.1.md](design-0.1.md), [receivers.md](receivers.md) |
| 9 | One agent contract, over MCP and as JSON over HTTP | [design-0.1.md](design-0.1.md), [agents.md](agents.md) |
| 10 | The job queue in a schema of its own | [design-0.1.md](design-0.1.md) |
| 11 | Three shapes held by tests: an owner that is not a superuser, an embedded host with its own River, two instances on one database | |

0.1.1 followed with the fixes from a review of the whole code base, among them two guards in the database (migrations `0002` and `0003`).

## Next: for a first pilot

In this order. A dry run needs no receiver, so the first three serve the dry run, and the receivers are written while it runs.

### For the dry run

1. **A request a reviewer can read.** Payloads carry ids, by design, and a list of ids is hard to judge.
   - An owner gives each payload field a label, and where it helps a link pattern, such as the order's page in the back office. The inbox shows the label and the link beside the value.
   - The label and the pattern are the owner's. The agent's value fills one encoded part of the path or the query, never the scheme or the host.
2. **A verdict on a dry run, with a reason a program can read.** A person can only set a dry-run proposal aside today, so a dry run tells a team what its mandates would allow, and nothing about what its people would have said.
   - When setting one aside, a person records what they would have done: approve, reject or edit, with a reason from a short list and an optional note. It decides nothing, and nothing is carried out.
   - A live rejection gets the same reason. An agent, and the figures, can then tell "wrong amount" from "not our policy".
   - Without this, the next item has nothing to set a rule against in a pilot that starts in a dry run.
3. **A mandate tried against the record before it is granted.** A person about to grant a mandate sees what it would have done.
   - A draft mandate (actions, risk, condition, budget) is replayed over the tenant's past proposals, dry runs included. Nothing is granted and nothing is written.
   - The answer sets the rule against what people decided, or said they would have: how many it would have allowed, how many it would have left to a person, and how many it would have allowed that a person rejected or edited.
   - The database gives the answer, with the functions a real execution uses, so the replay is not a third implementation of the rules. The differential test under [Evidence](#evidence-alongside) holds it to them.
   - **A mandate's states are settled in the same note,** since they belong together: a draft that is tried; *in shadow*, where it records what it would allow while a person still decides; in force; suspended by a person and lifted again; and ended. Today a mandate is granted and revoked, and a revocation is final.
   - A long dry run outgrows today's lists, which stop at the newest few hundred rows. The replay reads the whole record.
   - What it is not: proof that a rule is right. An approval is not evidence that a proposal was correct; rejections and edits are the signal.

### Before an agent goes live

4. **A receiver kit in Go, and a test any receiver can run.** The receiver is the work every system needs, and where a mistake does damage.
   - **The kit:** `examples/webhook` keeps the ids it has seen in memory, so a restart forgets them. The kit keeps them behind a small store interface, with a durable example. It also shows the two guards a receiver owes itself:
     - doing a thing once by the system's own fact, such as the order refunded, not only by the delivery id, since two proposals for the same thing are two deliveries;
     - its own caps per day and per key, which hold whatever was approved.
   - **The conformance test** sends a receiver, in any language, what it must refuse and what it must not repeat:
     - a bad signature, an unknown key id, another audience, another tenant
     - a delivery past `notAfter`
     - a repeated delivery, which must have no second effect
     - a refusal shaped so pgmandate keeps it, and outcome unknown

     A test from outside cannot see an effect, or make a receiver refuse. So the receiver under test carries one test action that counts its calls and answers as asked.
5. **Limits per key, on a mandate.** A budget counts all of a mandate's executions in a calendar month, whoever they are for. It cannot say "at most one per customer per quarter", so one customer, or one agent's mistake, can use all of it.
   - An action names the payload field that says whom a proposal is for, such as a customer or an account. The name is fixed on the proposal when it is made. It must be the field the receiver acts on, and a proposal without it waits for a person.
   - A mandate may limit executions per value of that field, per window. The database counts them under the mandate's lock, on the payload executed.
   - A dry run spends nothing, so today many dry runs can each show *would allow* against the same last place in a budget. A dry run keeps a tally of its own, per key and per month, so its figures do not overstate what a mandate would allow.
   - Built as a count. [Budgets in money](#expected-by-adopters) use the same mechanism, an action naming a payload field, so the design note covers both and money is built when a team needs it.
6. **Sessions that end with the person.** A session lasts a week by default and is not checked against the sign-in provider, so a person disabled there keeps their access until it runs out.
   - For a pilot: `MANDATE_SESSION_TTL` set to a working day.
   - The design note settles the rest. pgmandate keeps no token from the provider today, so tying a session to it means either keeping one or sending the person through the provider again, unseen. The threat model's line on sessions is restated with it.

## Evidence, alongside

Claims the project makes, turned into tests and write-ups. None of it adds a feature. In this order:

1. **The two evaluators fuzzed against each other.** Built. A condition is evaluated in Go and again in SQL, and the log's numbers are written in Go and read as `jsonb`. Tests make conditions, plans, payloads and numbers from a seed, run each through both sides, and fail on any difference; two fuzz targets cover the logic that needs no database. A normal run is short, and a hunt runs millions of cases. The first runs found four things, each fixed with its case kept in the tests ([decisions.md](decisions.md)):
   - the database's check of a plan answered NULL for a plan with a field missing, and a CHECK constraint passes on NULL
   - the same check took a plan with a field of another operator, which the service cannot read
   - the compiler took an empty field name, which the database's check refuses
   - the canonical form left one kind of number unexpanded that `jsonb` spells out, so its digest changed through the database

   None let a mandate allow what it should not. Compiled conditions agreed between the two evaluators from the first run.
2. **Model-based tests against the database.** A small reference model of a proposal's and a delivery's states, written by hand in Go. A test fires random sequences of operations straight at the database, under random scopes: an agent, a person, the system, another tenant. After each step it checks what must always hold:
   - a decided proposal stays decided
   - a mandate's spend never passes its budget
   - a delivery that may have been sent never becomes *not sent*
   - the chain verifies

   One operation at a time first; two at once is the hard part and comes second. Today's tests that go past the Go code are written by hand, one case at a time.
3. **An upgrade test from a database with rows in it.** A 0.1.1 database with proposals, decisions and a log is upgraded, and the chain still verifies. It exists before any migration reaches a pilot's database.
4. **Numbers for the log's lock.** Appending to a tenant's log takes one lock. How many decisions a second that allows is measured and published, before anything is changed.
5. **An attack on its own privilege model, written up.** The threat model ends with "no independent person has tried to break it". Until one has, the author tries: the setting that allows a purge, the functions that run with their owner's rights, advisory locks any session can take, the check on a receiver's address. Each finding gets a test that fails before its fix. An outside review still comes after.
6. **Web tests that exercise behaviour.** Most of the web app's tests match the source text. They are replaced by tests that render a component or drive a browser, a page at a time, as pages are touched.

**Upkeep, done:** the release image is built by cross-compiling, without an emulator, and the build has a time limit. A release build hung under emulation once. Every pull request that touches the image now builds it for both architectures, as a release does.

## Being found

The project is of use only to teams that know it exists and can judge it quickly. Writing loses to code unless it is tied to it, so each thing that ships gets its write-up:
1. **The fuzz and the model-based tests:** what they found, and how a claim became a test.
2. **The replay:** trying a rule against people's verdicts before granting it.
3. **The pilot,** as a short case study with its figures, such as how often people agreed with the agent unedited and what share a rule would have covered, whatever they show, and where the team agrees to it.

Beside those:
- **The design written up:** the database as a second gate and the mandate as a bounded grant, for people who will never run it.
- **How it was built:** with AI coding agents, and how their work was checked.
- **The contracts as a short specification** of their own: the mandate as a grant, and the delivery contract with its test vectors. A reader should be able to build a receiver, or a second implementation, from it.
- **Experience fed back to the standards work,** where a running implementation is useful input: the IETF's individual drafts on agent audit records and on the history of an agent's authority, and MCP's work on tool risk annotations. pgmandate builds against a standard once it is final, and exports to a draft's format only as a versioned adapter.

## After the pilot: a list to choose from

Each item has a reason to exist. Which ones get built, and when, follows what the pilot uses and runs into.

### Not yet common

Ideas from research and practice that fit the scope's [principles](scope.md#principles). In November 2026 they were found in papers and prototypes, or shipped in a form where a model decides, as in the coding agents' automatic approval modes.

- **The record as feedback.** An agent should not propose what people reject, over and over. The record may make an agent's *proposals* better; it never widens what the agent *may do*. Only a person does that.
  - **Its own outcomes, in a form to learn from.** The agent already reads the decision, the note and the payload as decided. The reason from the [verdict](#for-the-dry-run) is added, and what a person changed when they edited.
  - **A repeat is seen.** A proposal that matches one a person rejected is marked as such for the agent and for the reviewer, with the earlier note.
  - **Guidance per action, written by an owner,** returned with the action list. It is the owner's text, so the agent may trust it.
  - **Figures per agent, model and version:** approved unedited, edited, rejected. They are for the team that tunes the agent.
  - **What it must not become:**
    - pgmandate does not train or run agents.
    - Notes go back to the agent and its model provider, so they keep to ids.
    - An agent reads only its own outcomes, unless an owner shares an action's.
    - An agent that learns what gets approved can learn to persuade, which is why a mandate rests only on facts a receiver checks.
- **A mandate that stops itself.** An owner sets what suspends a mandate: receivers refusing its deliveries, people rejecting or editing what it would have covered, or a monitor's alarm. A suspended mandate allows nothing until a person lifts it, and both are logged. It is the suspended state a person can set by hand.
  - A model may be one of the monitors. It can only stop a mandate, never allow a proposal. A flag works the same way today: it keeps a mandate from allowing the one proposal it is on.
  - Someone who can trigger a suspension can stop work, so the thresholds are the owner's, and a suspension sends everything to people rather than refusing it.
- **A hold, and a declared reverse.**
  - An action may set a hold: what a mandate allows waits that long before it is delivered, and a person may cancel it meanwhile.
  - An action may name the action that reverses it. "Undo" then makes a linked proposal, decided like any other.
  - An owner can keep an action with no reverse from every mandate.
  - A reverse is a second action, not an erasure, and the documentation says so.
- **Facts a receiver has attested.** Today a condition decides on what the agent wrote. A receiver could answer a check with signed facts, the proposal carries them, and a condition can require that a field is attested and not typed by the agent. The receiver still checks again when it writes, since facts go stale.
- **The log's latest hash, receipted outside the database.** The database's owner could rebuild the log. A signed statement of the latest hash, registered with a transparency service (the IETF's SCITT), and the receipt kept, lets anyone show that the log they see is the one that was. Publishing the hash anywhere outside is the first step.
- **Figures on the reviewing itself:** time to decide, how often a proposal is edited, the share carried out without a person. They show a team whether its review is real. They are about the tenant's process, and what is shown per person is the owner's choice and subject to workplace rules.
- **Suggested mandates from past decisions,** never a one-click grant. The replay under [Next](#for-the-dry-run) is what a suggestion is shown with.

### Expected by adopters

- **Waiting without polling.**
  - **A callback when a proposal is decided**, so a run that suspended resumes without polling:
    - An owner registers the address per agent; an agent never chooses where pgmandate calls. The address is held to the same checks as a connector's.
    - It is signed with the deployment's key, under an audience of its own, so a callback can never pass for a delivery.
    - It carries only "proposal X changed"; the host reads the rest with its own token.
  - **As an MCP Task.** Tasks are now an official MCP extension. `propose` can return a task that is *working* until the proposal is decided, with the expiry as its lifetime, and cancelling it withdraws the proposal. A proposal that waits for a reviewer is not `input_required`: that is for input from the agent's own client.
  - **The MCP server checked against the 2026-07-28 specification,** whose core is stateless: the client's information, which provenance is read from, now comes with each request, or not at all.
- **Clients in Python and TypeScript**, generated from the agent contract.
- **One framework adapter, for the first team's framework, in a form that governs.**
  - The governed tool's body proposes and waits for the receiver's result. The agent's process never holds the system's credential.
  - Waiting hours for a person is what durable execution does well. Where a team runs Temporal or Restate, the natural form is an activity that proposes, with the decision arriving as a signal through the callback above.
  - An adapter that only answers a framework's approval hook (`needsApproval`, `canUseTool`, `interrupt`) pauses a tool that then runs with the agent's own credential. That is recorded approval, not governance, and it is offered, if at all, under that name.
- **Receiver templates in Python and TypeScript,** held to the conformance test above.
- **Deciding from a phone.**
  - A decide page that reads well on a phone, reached from the notice's link.
  - A reminder before a proposal expires, and a second person told when the first has not decided.
  - **A plain webhook notifier**, so any chat tool or paging system hears when something waits. It still carries none of the agent's text.
- **Notifier addresses encrypted** under a key outside the database. They are bearer capabilities, and they sit in every backup today.
- **Budgets in money**, as a second limit beside the count.
  - The limit is an amount per currency, with no exchange between currencies.
  - An action names the payload fields that carry its cost and currency. Those names are fixed on the proposal when it is made, so an owner cannot remap them before a decision. The database reads them on the payload executed, as it does a condition.
  - A cost that is missing, negative, not a number, or in a currency the mandate has no budget for waits for a person.
  - The cost field must be the field the receiver carries out. Otherwise an agent could state a small cost and have a large amount paid. The conformance test checks it, and the threat model's "cannot understate the cost" is restated for it.
  - It caps an agent splitting a large request into small ones at the month's budget. It does not stop the splitting.
- **Two approvers for high risk:** two different people, each recorded.
- **A database role of its own for the part that delivers.** Today any session of the application role can declare itself the system. A separate role narrows that, and is the first step toward a scope the application cannot declare for itself.
- **The signing key in a key management service,** as an option. No cloud becomes required.
- **Erasure of personal data from a payload while the log still verifies.** Digests in the log become keyed per proposal; erasing drops the key and the payload. The design note comes early, since it changes the log's format and must say what happens to rows that exist.

### When a team has the need

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
  - the application role as a group role; this needs care, since a host's whole application would then act with pgmandate's rights
- **People:**
  - more channels: Microsoft Teams and email
  - routing by action or department
  - approving from the chat, bound to the payload's digest, never tied to a person by name
  - the inbox as a triage tool: grouped by why proposals wait, filters, a "mine" view, and no bulk approve
  - departments as scopes
  - passkeys
  - an idempotency key on the people API, so a grant sent twice is one mandate
- **Conditions:**
  - the rest of CEL, evaluated by the service, with the database checking that the condition cited is the mandate's own
  - a *refused* outcome for what must never happen
  - limits on a total within windows shorter than a month
  - a condition built from the action's schema, field by field, for owners who do not write CEL
- **Agents:** typed tools, each action offered as its own MCP tool. Action keys contain dots, which some clients refuse in tool names, and the payload must sit under a field of its own so it cannot collide with the rationale or the idempotency key.
- **The record:**
  - an inventory: every agent, what it may do, what it spent, how much ran without a person
  - exports as an evaluation dataset. Every decision is a person's verdict on an agent's proposal: approved unedited, edited, or rejected, by model and version. That is a labelled set for evaluation tools such as Langfuse, Arize Phoenix or Braintrust.
  - a feed for security monitoring
  - an export in the format of an IETF agent audit draft, as a versioned adapter; the drafts are individual and still moving, so none becomes the log's own format
  - the database checking that each decision writes its event, and the event's hash
- **Scale, and not a choke point:**
  - Failing closed is right for changes, and reads never pass through pgmandate. It stays optional per system: a team sends through it only the changes worth governing.
  - several instances with a replicated Postgres, and a latency budget
  - pagination and streaming exports
  - a narrower lock on the log, once it is measured
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
- **A decision endpoint for someone else's gateway.** Answering a gateway's "is this call allowed?" would leave the credential at the gateway. By the scope's own terms that is recorded approval, not governance.
- **A model that approves.** A model may stop a mandate, and its output may be shown to a person as a claim. It never allows a proposal, and it never widens a mandate. A track record may suggest a wider mandate; a person grants it.
- **A switch that fails open.** When pgmandate cannot be reached, a governed change does not happen.
- **A risk score worked out by the system.** Risk is what an owner set on the action, so a person can say why a proposal waited.
- **Storage other than Postgres, and a Postgres extension.** The database as the second gate is the point. Plain migrations run on managed Postgres; extensions often do not.
- **Signed mandates an agent carries.** They would give the agent a credential again, and they cannot be revoked at once or counted against a shared budget offline.
- **Infrastructure for running agents:** sandboxes for agent code, agent memory, training, messaging between agents, caching and model routing. pgmandate governs what agents change.
