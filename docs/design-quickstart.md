# Design note: one image and a ten-minute quickstart

Status: built, 2026-11-25. Roadmap item 1. When the code differs from this note, one of them is wrong.

## The goal

With only Docker installed, a newcomer goes from nothing to signed in to the UI, with an agent proposing and a decision made, in ten minutes. That needs neither Go, Node nor a sign-in provider.

Today it does:
- **Go and Node,** for `make run`.
- **A sign-in provider,** for the server in compose. Dev login answers only on loopback, and a container listens on every address.
- **A signing key and an app password,** both set by hand.

## What changes

### 1. A published image

- **Where:** `ghcr.io/andreas-lindfalk/pgmandate`, built by GitHub Actions when a tag `v*` is pushed.
  - Tags `vX.Y.Z` and `latest`.
  - `linux/amd64` and `linux/arm64`.
  - Built with `VERSION` set to the tag, so the image reports its release.
- **Permissions:** the workflow pushes with the repository's own token (`packages: write`), and nothing else needs a secret. The package takes the public repository's visibility, so it is public from its first publish.
- **Before the first release** there is no image to pull. The quickstart then builds from the clone (`--build`), which needs Docker only.

### 2. A setup link instead of dev login

- **When the server makes one:** when it starts and the database has no tenant yet. It writes a single-use link to its log:
  `Set up pgmandate: http://localhost:8585/setup#<token>` (valid for 30 minutes)
- **What the page asks:** the organisation's name, your name and your email.
  - Submitting it creates the tenant and you as its owner, and signs you in. All of that happens in one transaction that also marks the link used.
- **The token:**
  - 32 random bytes, stored only as a hash, and used once.
  - It works only while there is no tenant, and it expires after 30 minutes.
  - Each start makes a new one and voids any older one that is unused.
  - It travels in the URL fragment, so it never reaches a server log, a proxy or a `Referer`.
- **Why it is safe enough:** anyone who can read the container's log can set up the first tenant. That is the operator, the same person who can already run `pgmandate tenant create` in the container. Once a tenant exists, no setup link works again.

### 3. Sign-in links for a person, from the command line

- **The need:** an owner signed in by a setup link has no other way back in when the session ends (a week), unless a provider is configured.
- **The command:** `pgmandate sign-in-link -tenant ID -email E` prints a single-use link for an existing person, valid for 15 minutes:
  `http://localhost:8585/sign-in#<token>`
  In the quickstart that is `docker compose -f deploy/quickstart/compose.yaml exec pgmandate /pgmandate sign-in-link ...`.
- **The session it gives** is pinned to that one person: it reaches their tenant and no other, and binds nothing. That matches the sign-in note, which says a link session names its person directly.
- **Who can make a link:** whoever can run the command line, which already acts as any person. Nobody gains anything new.

### 4. The quickstart

- **Where:** `deploy/quickstart/compose.yaml`, with Postgres and the image.
  - The UI is published on `127.0.0.1:8585` only.
  - Postgres is not published at all.
- **How:** one command, `docker compose -f deploy/quickstart/compose.yaml up`, then open the setup link from the log.
- **What the README walks through:** connecting an agent with the token the UI issues, a mandate, and a proposal decided.
- **No secrets to set by hand:**
  - **The signing key** is made once by the server into the quickstart's volume. This uses a new opt-in, `MANDATE_SIGNING_KEY_FILE` together with `MANDATE_SIGNING_KEY_CREATE=true`: if the file does not exist, the server writes a new key there with mode 0600 and logs that it did. Without `..._CREATE`, a missing file still stops the server, as now, so a mistyped path never quietly makes a new key that receivers do not trust.
  - **The app password** is a fixed value in the quickstart's compose file. The database is reachable only from the compose network, and the README says so. The check that refuses the default `app` beyond loopback stays as it is.
- **Not for production.** The quickstart's README section says so, and points to "Running it" for that.

### 5. CI runs the quickstart

On every pull request that touches `deploy/` or the server, a job:
1. builds the image and starts the quickstart;
2. reads the setup link from the log and sets up a tenant through the API;
3. issues an agent token, has an agent propose and a person approve, all over HTTP;
4. checks the result.

That holds the "ten minutes with only Docker" promise in CI rather than in a README.

## Schema

- **`sign_in_links`** (global, like `sessions`):
  - `token_hash`
  - `purpose` (`setup` or `person`)
  - `person_id`, empty for `setup`
  - `expires_at`, `used_at`, `created_at`
  - Append-only except `used_at`, which is set once.
  - The app role may insert and mark one used, and reads only by hash.
- **`sessions.person_id`**, empty except for a link's session, which then reaches only that person.
- **Changes allowed only as the system:** a setup link is made only while no tenant exists. Redeeming it creates the tenant, its first owner and the session in one transaction.

## The API

- **`AuthService.SetUp(token, organisation, name, email)`**
  - Public, like `DevLogin`.
  - Returns a session and sets the cookie.
  - Refused once a tenant exists, or for a used, expired or unknown token. All three are the same `unauthenticated`, so nothing tells them apart.
- **`AuthService.RedeemLink(token)`**: the same, for a person's link.
- **Web:** a `/setup` page with the three fields, and a `/sign-in` page that redeems at once. Both read the token from the fragment.

## Tests

- **The setup link:**
  - made only when there is no tenant; a second start voids the first;
  - refused once used, once expired, and once a tenant exists;
  - two redeems at once make one tenant.
- **A person's link:**
  - pinned to that person and tenant: another tenant where the same email is a person is not reached;
  - refused after use and after expiry.
- **Stored hashed:** the token never appears in the database, and the log shows only the link.
- **The signing key:**
  - with `..._CREATE` the file is made once and reused on the next start;
  - without it, a missing file stops the server.
- **The quickstart, end to end in CI,** as above.

## Settled in review

1. **The registry:** `ghcr.io/andreas-lindfalk/pgmandate`, built when a `v*` tag is pushed, by `git push` or by publishing a release on GitHub, for both architectures. The package turned out public from its first publish, as the repository is.
2. **Lifetimes:** the setup link 30 minutes, a person's link 15 minutes, and a session from either one the usual week.
3. **`pgmandate sign-in-link`** is built. Without it, a quickstart whose session has run out must start over.
4. **The CI job** runs on every pull request that touches `deploy/`, the server, or the web app, and on `main`.
