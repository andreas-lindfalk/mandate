# helpdesk: a Go service that embeds pgmandate

A small ticket system that runs pgmandate in its own process and on its own database. Its actions are carried out by its own code, in the same transaction that decides them, so there is no webhook and no receiver. This is the shape for a Go service that wants an inbox and mandates in front of its own writes. [docs/scope.md](../../docs/scope.md#embedded-in-a-go-service) says when to choose it.

## Try it

The test suite uses it as its host; the kernel's tests show it at work:

```sh
go test -timeout 300s -run TestKernelSuite .
```

A host wires it up as [internal/testfix](../../internal/testfix/testfix.go) does: apply `helpdesk.Migrations` after pgmandate's, create the kernel with `pgmandate.New`, and `Register` each of `helpdesk.Actions()`.

## Reading it

1. [schema/0001_tickets.up.sql](schema/0001_tickets.up.sql): the host's own table. Every row names the proposal behind its latest write (`last_proposal_id`). Row level security, through `mandate.may_write_for`, lets a row be written only by a person, or by the system while it executes exactly that proposal. No agent can touch a ticket, and the system cannot for a dry run.
2. `Actions()` in [helpdesk.go](helpdesk.go): each action's key, payload schema and risk, and whether a mandate may allow it. `refunds.issue` also has `Assess`, which sets the risk from the payload: medium below 100, high from 100, so a person always decides a large refund.
3. `createTicket`, `closeTicket` and `refund`: the `Apply` functions. They write in the transaction they are given, the one that decides the proposal, so the write and the decision commit together or not at all.

## Before you copy it

- Your tables need the same guard: `last_proposal_id`, the foreign key to `mandate.proposals`, and the `may_write_for` policies. Without them, the system could write rows for a proposal it is not executing, and the database would not refuse it.
- One River version runs per binary. If your service uses River too, see [docs/design.md](../../docs/design.md).
