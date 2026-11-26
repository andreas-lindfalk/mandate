# Examples

A decided proposal is carried out in one of two ways, and the examples show both:

- **Over HTTP.** pgmandate posts the decision, signed, to a *receiver* next to your system, which holds the system's credential and does the write. This is the main shape: the agent's side never holds the credential, and the receiver can be written in any language.
- **Embedded.** A Go service runs pgmandate in its own process, on its own database, and carries the action out in its own code, in the same transaction that decides it.

| Example | Way | What it shows | Where it runs |
|---|---|---|---|
| [webhook](webhook) | Over HTTP | A reference receiver of delivery contract v2: it verifies the signature, checks it is the audience and tenant, claims the delivery id, keeps to `notAfter`, and answers with an explicit outcome | `pgmandate receiver`, and the demo's stand-in helpdesk |
| [refunds](refunds) | Over HTTP | A receiver for an action that moves money: every fact the agent claims is checked against the shop's own records first, and each refund is paid once | Its own tests |
| [helpdesk](helpdesk) | Embedded | What a host provides: its own tables, guarded so only a decided proposal writes them, and actions with a schema, a risk and the code that applies them | The test suite, as its host |

Each is short, and its Go doc comment says what it does and why. The README in each folder says where to start reading and what is only there for a demonstration.

There is no example of an agent: any MCP client is one. [docs/agents.md](../docs/agents.md) shows how to connect one, the demo (`make demo`) drives one over MCP, and `pgmandate propose` is one on the command line.
