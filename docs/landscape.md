# Landscape

What others ship that overlaps with pgmandate, going by their own documentation as of November 2026. What is particular to pgmandate, and what it shares with others, is in [scope.md](scope.md#what-it-brings).

| Approach | Examples | What it decides | Relation to pgmandate |
|---|---|---|---|
| Gateways and cloud agent platforms | AgentCore Policy (Cedar, and temporal policies in Dogwood), agentgateway, Docker, Permit.io and TrueFoundry MCP gateways | Allow, deny or ask, per tool call, in front of the tool. Many hold the tool's credential. AgentCore's temporal policies also count and sum calls within one session, over at most 24 hours | Complementary, and overlapping where a gateway holds the key. A mandate is a named person's grant over months, with a policy reference, on your own Postgres, and a person decides what it does not cover |
| Framework approval steps | OpenAI Agents SDK, LangChain/LangGraph, Claude Agent SDK, Temporal and Inngest waits | A pause for a person inside one run | Complementary. pgmandate gives them standing permission, a budget and one record across frameworks |
| Protocol and tool prompts | MCP elicitation | The tool server asks the user before it acts | Complementary. No standing permission and no shared record |
| Human review services | gotoHuman | A review request as data, a webhook when it is done | Close in shape, and a hosted service. pgmandate adds standing mandates with a budget, on your own database |
| Identity, vaults and asynchronous approval | Microsoft Entra Agent ID, Okta, Auth0 for AI Agents, Keycard, Infisical Agent Vault | Who the agent is and where the key lives. Auth0's asynchronous authorization also asks a person to approve one specific transaction | Overlapping. Auth0 closes the first gap in [the problem](scope.md#the-problem); pgmandate adds standing mandates, budgets and a record of authority |
| Policy engines | Cedar, OPA, Cerbos, OpenFGA | Yes or no per request | Complementary. No queue for a person and no record of authority |
| Governance libraries | Microsoft Agent Governance Toolkit | Policies in the agent's process, with budgets, approvals with quorum, and a chained audit log | Overlapping. It runs inside the agent; pgmandate runs outside it, on a database the agent cannot reach |
| Approval layers of the same kind | SidClaw, Countersign, OpenLeash, withHuman, Placet, awaithumans | Policy, approval and a hash-chained audit around an agent's tool calls; OpenLeash lets agents propose policies to their owner | Closest in intent. Mostly in front of the tool or in the agent's process; pgmandate's difference is the database as a second gate and the mandate as one bounded grant |
| Pre-approved changes | ITSM standard changes, such as ServiceNow | Change types allowed without review | The same idea for IT operations. pgmandate applies it to agents' actions |
| Payment mandates | Google's AP2 and Verifiable Intent | Signed spending limits for purchases | The nearest relative, and the source of the word. It covers payments, and the agent carries it as a credential |
