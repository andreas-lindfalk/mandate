# Running pgmandate

From a first tenant on your own machine to a server you run for real, and how a release is made. The front page is the [README](../README.md).

## The demo, step by step

`make demo` sets up a tenant, an owner, an agent, four webhook actions and a mandate, with a small receiver standing in for a helpdesk. The agent then proposes over MCP, exactly as any MCP client would. The demo prints fourteen steps:

1. The setup: tenant, owner, agent, four actions, one mandate for `tickets.*` up to low risk with a budget of three executions a month.
2. The agent asks what it may do.
3. A ticket within the mandate is allowed at once, delivered with a signature the helpdesk verifies, and the helpdesk's answer comes back to the agent. Sent again under the same key, it is a repeat, and nothing is done twice.
4. Closing a ticket is medium risk, above the mandate: it waits for a person, and the reason is recorded.
5. A call-back is outside the mandate, and its action lets a proposal wait only one minute. Nobody will decide it.
6. A ticket whose text matches a known attempt to steer a model is flagged, high risk, and waits.
7. A refund is high risk, and high risk is never delegated: it waits.
8. Two more tickets use up the budget; the next one waits. One delivery is made to fail once, to show the retry.
9. The owner approves one waiting proposal and rejects another.
10. A second agent, new and so in dry run, proposes over MCP: a mandate would allow one proposal, and nothing is delivered or spent; the owner dismisses the other.
11. The call-back's minute is up. The agent reads it as expired, the owner can no longer approve it, and nothing is delivered.
12. Five statements that no rule allows are sent straight to Postgres, as the system, the owner and the agent. Each is refused, with the database's own message.
13. The log verifies.
14. The first decided proposal is shown as it is exported.

It ends with the commands for what to do next:

- `make run`, open http://127.0.0.1:8585, sign in as `alice@acme.example`, and you are in the inbox the demo left behind.
- The demo's receiver stops with the demo. Start the one it prints (`go run ./cmd/pgmandate receiver ...`) in another terminal, so that what you approve has somewhere to land. Without it a delivery retries and then shows as not delivered, and you can send it again before its deadline.
- The token the demo prints connects your own agent ([agents.md](agents.md)). Its budget is spent, so grant a new mandate in the UI to see proposals go through by themselves.

Each `make demo` adds another tenant with the same name; `make reset` starts clean. `make demo-ui-gifs` records the README's films of the UI: from a clean `make reset && make up && make demo`, with `make run` and the demo's receiver running, as `MANDATE_AGENT_TOKEN=<the token it printed> make demo-ui-gifs`. It needs Playwright's Chromium (`cd web && pnpm exec playwright install chromium`) and ffmpeg, and since it approves the demo's refund and grants a mandate, each recording starts from that clean reset again ([web/scripts/record-ui.mjs](../web/scripts/record-ui.mjs)). `make demo-terminal-gif` records its terminal film (with vhs and tmux).

## The quickstart's sign-in

The Docker quickstart's setup link works once, for 30 minutes, and only while nothing is set up; a restart prints a new one. When your session runs out, get back in with a single-use link:

```sh
docker compose -f deploy/quickstart/compose.yaml exec pgmandate /pgmandate sign-in-link -tenant <id> -email <you>
```

`deploy/quickstart/smoke.sh` walks the quickstart through the APIs, a receiver in the container included, and CI runs it.

## Your own agent, your own system

From the repository, as the first owner:

```sh
make up && make migrate                    # Postgres, and the schema and the app role in it
go build -o pgmandate ./cmd/pgmandate      # the command line, as ./pgmandate in any terminal here

./pgmandate tenant create -name Acme -owner alice@acme.example -owner-name Alice
# tenant 7d0c…-…  (Acme)
# owner  …
T=<the tenant id it printed>

# What the agent may ask for, and where it goes. It prints the action's audience.
./pgmandate action define -tenant $T -as alice@acme.example -key tickets.create -risk low -may-execute \
  -url http://127.0.0.1:8586/ -schema '{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}'

./pgmandate agent create -tenant $T -as alice@acme.example -name helpdesk-bot -mode live   # prints the agent id and its token
./pgmandate grant -tenant $T -as alice@acme.example -agent <agent id> -actions 'tickets.*' -max-risk low -budget 20 -policy "AI policy v1"

make run                                   # then connect the agent (agents.md)
./pgmandate receiver -tenant $T -audience pgm:...   # in another terminal, once the server runs: your system, for now
```

Locally the server makes its signing key once, in `.local/signing-key`, and the receiver fetches its public half from the running server.

The agent's `tickets.create` proposals are now allowed and delivered, twenty a month; everything else waits in the inbox at http://127.0.0.1:8585. Without `-may-execute` a person always decides, and without `-agent` a mandate covers every agent of the tenant. `action define` sets the whole definition each time: a flag left out goes back to its default. Replace the receiver with your own ([receivers.md](receivers.md)).

A new agent starts in a dry run unless it is created with `-mode live`: see [a dry run first](agents.md#a-dry-run-first).

## Running it

One image and one Postgres. Each release is published as **`ghcr.io/andreas-lindfalk/pgmandate:<tag>`**, such as `:v0.1.1`, and `:latest` follows the newest. It is built for linux/amd64 and linux/arm64, with the web app inside. Its entrypoint is the `pgmandate` command, so `docker run --rm ghcr.io/andreas-lindfalk/pgmandate:v0.1.1 version` prints the release; without arguments it serves.

The compose files run that image. A one-off `migrate` container holds the database owner's credentials; the server connects only as the application role:

```sh
export MANDATE_APP_PASSWORD=$(openssl rand -hex 16) MANDATE_OWNER_PASSWORD=$(openssl rand -hex 16)
export MANDATE_SIGNING_KEY=$(openssl rand -base64 32)
export MANDATE_VERSION=v0.1.1                         # the release to run: the image's tag
export MANDATE_OIDC_ISSUER=... MANDATE_OIDC_CLIENT_ID=... MANDATE_OIDC_CLIENT_SECRET=...
docker compose -f deploy/compose.yaml -f deploy/compose.server.yaml up -d   # --build builds this clone instead
```

Keep both passwords and the signing key, in an `.env` file next to the compose files or a secret store: the owner password is fixed when the database volume is first created, and each start of `migrate` sets the app role's password again. This stack has its own database volume, apart from the one `make up` uses. Dev login is refused here, since the server does not listen on loopback. Until a tenant exists, the server prints a single-use setup link to its log, valid for 30 minutes, and whoever opens it makes the first tenant and its owner: create the tenant right away, or open that link yourself. Without a provider, `pgmandate sign-in-link` in the `migrate` service gets a person back in (add `-e MANDATE_PUBLIC_URL=...` when the server is not at `http://127.0.0.1:8585`). A receiver on the same machine is `http://host.docker.internal:<port>` from inside the container, a private address that needs `MANDATE_ALLOW_PRIVATE_CONNECTORS=true`; on Linux the receiver must listen on `0.0.0.0` (`-addr 0.0.0.0:8586`) to be reached that way. Then create a tenant with its first owner:

```sh
docker compose -f deploy/compose.yaml -f deploy/compose.server.yaml run --rm \
  migrate tenant create -name "Acme" -owner alice@acme.example -owner-name Alice
```

The administrative commands act on the database as the application role: through `MANDATE_APP_DATABASE_URL` when it is set, otherwise at the address and with the app password worked out from the owner connection. Here they run in the `migrate` service.

From there the owner works in the UI. Locally the same command is `go run ./cmd/pgmandate tenant create ...`, and `go run ./cmd/pgmandate help` lists the rest.

| variable | meaning |
|---|---|
| `MANDATE_DATABASE_URL` | the owner connection, for `migrate`; defaults to the local Postgres |
| `MANDATE_APP_PASSWORD` | the password of the `mandate_app` role: given to the role `migrate` creates, and set on an existing one only when given. The default, `app`, is refused when the server listens beyond loopback |
| `MANDATE_CREATE_APP_ROLE` | default `true`: `migrate` creates `mandate_app` when it is missing. `false`: a DBA created it, and `migrate` only checks it. On Postgres 16 and later only whoever made a role may set its password, so leave `MANDATE_APP_PASSWORD` unset for a DBA's role |
| `MANDATE_REVOKE_PUBLIC_CREATE` | `true` revokes `CREATE` on the schema `public` from everyone. Off by default: Postgres 15 and later grant none, and a shared database may need it left alone |
| `MANDATE_OWNER_PASSWORD` | compose only: the database owner's password when its volume is first created, default `owner` |
| `MANDATE_AGENT_TOKEN` | the token `pgmandate propose` uses when `-token` is not given |
| `MANDATE_APP_DATABASE_URL` | connect as the app role directly. With it, and without the owner connection, `serve` does not migrate and never holds the owner's credentials. This is how to run it for real: `pgmandate migrate` as a separate step |
| `MANDATE_HTTP_ADDR` | where to listen, default `127.0.0.1:8585` |
| `MANDATE_PUBLIC_URL` | how the server is reached, for cookies and the sign-in redirect |
| `MANDATE_OIDC_ISSUER`, `MANDATE_OIDC_CLIENT_ID`, `MANDATE_OIDC_CLIENT_SECRET` | the sign-in provider. A person added by email is bound at their first sign-in to a tenant to the provider's identity for them, and from then on that identity, and no other with the same address, is them there |
| `MANDATE_OIDC_REDIRECT_URL` | default `<public url>/auth/oidc/callback` |
| `MANDATE_DEV_LOGIN` | `true` lets anyone sign in with an email. Refused unless the server listens on loopback, and then it answers only to requests for localhost |
| `MANDATE_SESSION_TTL` | default `168h` |
| `MANDATE_CLIENT_IP_HEADER` | the header a trusted reverse proxy writes the client's address in, such as `X-Forwarded-For` (its last entry is used). Failed sign-ins and token checks are limited to twenty a minute per client address; without this header, behind a proxy every client shares the proxy's address and so one limit. Set it only when the proxy overwrites the header |
| `MANDATE_ALLOW_PRIVATE_CONNECTORS` | `true` lets webhook connectors live on local and private addresses, for development and for an on-premise install. Off, a delivery to such an address is never sent, and ends *not sent* |
| `MANDATE_SIGNING_KEY` | the Ed25519 seed deliveries are signed with, 32 bytes in standard base64 (`openssl rand -base64 32`). Never stored in the database. Required unless the server listens only on loopback, where one is made once in `.local/signing-key` |
| `MANDATE_SIGNING_KEY_FILE` | the same, read from a file |
| `MANDATE_SIGNING_KEY_CREATE` | `true` makes the file named by `MANDATE_SIGNING_KEY_FILE` once, when it does not exist, with a new key readable only by its owner, as the quickstart does. Without it a missing file stops the server, so a mistyped path never makes a key receivers do not trust |
| `MANDATE_SIGNING_KEYS_PREVIOUS` | comma separated public keys, in standard base64, that are still published but no longer sign: how a key is rotated |
| `MANDATE_RECEIVER_ADDR` | where `pgmandate receiver` and the demo's receiver listen, default `127.0.0.1:8586` |

### On a managed Postgres

RDS, Aurora, Cloud SQL, AlloyDB, Azure, Supabase and Neon give you an admin role that is no superuser. Point `MANDATE_DATABASE_URL` at that admin, in a database it owns or may create in, and run `pgmandate migrate`. It creates `mandate_app` without the attributes only a superuser may set, or uses one a DBA made beforehand (`MANDATE_CREATE_APP_ROLE=false`). The admin owns the tables; the server connects as `mandate_app` only (`MANDATE_APP_DATABASE_URL`).

`migrate`, and `serve` when it starts, refuse to go on when `mandate_app` could get past the rules: when it may set `session_replication_role`, or is, or through membership can act as, a role that is a superuser, bypasses row level security, creates roles, replicates, is the service's admin (`rds_superuser`, `cloudsqlsuperuser`, `azure_pg_admin` and the like), is one of Postgres's own roles with the server's files or every table (`pg_execute_server_program`, `pg_read_all_data` and the like), or owns pgmandate's schemas or tables, or may truncate or add triggers to them (as `GRANT ALL ON ALL TABLES` allows). The error says which. Each of these gets past row level security or the append-only guard, and a role a DBA set up for convenience often has one. `migrate` also warns when the role may create objects in the schema `public`.

## Building the image yourself

The quickstart pulls the published image. To try a change of your own in it, build it from the clone instead:

```sh
docker compose -f deploy/quickstart/compose.yaml up --build
```

CI does this on every pull request that touches the server, the web app or `deploy/`, and walks it with `deploy/quickstart/smoke.sh`.

## Releasing

Push a tag, or publish a release on GitHub with a new tag:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The `release` workflow builds the image for amd64 and arm64 and publishes it as `ghcr.io/andreas-lindfalk/pgmandate:v0.1.0` and `:latest`, reporting `v0.1.0` as its version. The package takes the public repository's visibility, so anyone can pull it; its settings are under your profile's *Packages* tab, *pgmandate*, *Package settings*.
