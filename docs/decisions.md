# Decisions

Newest first. A decision that changes the rules also changes `AGENTS.md`.

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
