# Agents

How an agent connects, what it is told, and what whoever runs it should know. The front page is the [README](../README.md); the terms are in [scope.md](scope.md#the-terms).

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

The server tells the agent how to work with it. In short: call `list_actions`, then `propose` with an `idempotency_key` made from its run and step and a `rationale`, its account for the person who decides of what it found and why this action follows, with `evidence` the person can check (where it looked, an id to find it again, what it showed). The answer is `{"created", "repeat"?, "proposal": {…}}`, and the proposal's `outcome` and `next` say whether it was allowed, carried out, or waits for a person and why. This justification is kept with the proposal as written, shown in the inbox as the agent's claim, and named in the log by digest. A mandate never reads it. Each answer is both text for a model and structured content for code, and an error carries a code (`invalid_argument`, `conflict`, `permission_denied`, `not_found`, `rate_limited`, `unauthenticated`, `canceled`, `internal`). After `internal` or `canceled` a proposal may have been stored: the same call under the same key returns it.

The same contract is served as JSON over HTTP, for an agent or a script that does not speak MCP. It is defined in [proto/mandate/agent/v1/agent.proto](../proto/mandate/agent/v1/agent.proto). Payloads, results and refusals travel as JSON text (`payloadJson`), so every digit of a number is kept; over MCP they are objects. Errors use the Connect codes, with `already_exists` for a key used for another request, `failed_precondition` for withdrawing what no longer waits, and `resource_exhausted` with the seconds to wait. Only an agent's token opens it: a person's session is refused.

```sh
curl -s http://127.0.0.1:8585/mandate.agent.v1.AgentService/Propose \
  -H "Authorization: Bearer mnd_..." -H "Content-Type: application/json" \
  -d '{"action":"tickets.create","title":"Printer jams","idempotencyKey":"run-7:1","runId":"run-7",
       "rationale":"Three reports from floor 3.","payloadJson":"{\"subject\":\"Printer jams\"}"}'
```

For whoever runs the agent:

- **Give each run a run id**, in the prompt or the task. Keys are made from it, and a run that reuses one gets the earlier run's answers. An agent given none takes `suggestedRunId` from `list_actions`.
- **Allow tool calls at least five seconds.** `propose` waits up to two seconds for a mandate's decision, and `get_proposal` can wait longer when asked; some clients give up sooner by default.
- An agent's calls are limited to five a second per token, over MCP and HTTP together, with bursts of thirty, in each server instance; its proposals to 30 a minute and 300 an hour unless an owner sets other limits, which the database holds across instances. An agent sees its limits in `list_actions`, and a refusal says when to try again.
- **Withdrawing.** An agent can take back its own waiting proposal with `withdraw_proposal`, as when it proposes a corrected version; a person then never approves both.
- **Notifications.** An owner can have a Slack or Discord channel told when a proposal waits for a person (in the UI under *People*, or `pgmandate notify set`). The message names the action, its risk, the agent and why it waits, and links to the inbox; nothing the agent wrote goes to the channel.

To try an action without an agent, propose from the command line, over MCP like any client:

```sh
go run ./cmd/pgmandate propose -token mnd_... -action tickets.create -title "Printer jams" -rationale "Three reports from floor 3." -payload '{"subject":"Printer jams"}'
```

It prints the answer and the run and key it used; the same `-run` and `-key` again is a repeat.

## A dry run first

A new agent starts in a **dry run**, unless it is created live (*Live from the start* in the UI, `-mode live` on the command line). That is how a team starts before it takes the agent's credential away:

- The agent keeps its own credential and goes on acting with it. It proposes what it is about to do, and every answer says `dryRun: true`. The answer that made the proposal tells it to do it itself, once; a repeat or a later read never asks it to act again.
- pgmandate records what a mandate would have allowed (*would allow*, with the basis) and what would have waited for a person. Nothing of a dry run is ever carried out: the database refuses to approve, execute or deliver one, to count it in a budget, or to let it open an embedded host's table. A waiting dry run sends no chat notice, and a person may only dismiss it.
- The inbox keeps waiting dry runs in a tab of their own, out of the queue a person works, and the log tags each of their events with `dryRun: true`. The decisions export leaves them out.
- An owner makes the agent live (`pgmandate agent mode -mode live`, or *Make live* in the UI) once the record shows what the mandates would do. Proposals made before keep what they were.

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
- **The receiver must remember delivery ids durably.** Use the id as the idempotency key of whatever you call next, or store it in the same transaction as the work. [examples/webhook](../examples/webhook) keeps them in memory, which is enough for a demonstration and not for a restart.
