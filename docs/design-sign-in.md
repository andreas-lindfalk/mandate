# Design note: sign-in that holds up

Status: built, 2026-11-25. Roadmap item 7. When the code differs from this note, one of them is wrong.

## The problem

A person is a row per tenant, found by email. On every call the server takes the email from the session and looks up the person with that email in the tenant the call names. The provider's subject id is never kept. So:

- **An email that changes hands takes the person with it.** If the provider gives the address to someone else (a leaver's mailbox reassigned, an alias moved), the new holder signs in as the old person, owner rights included.
- **Only one provider can ever be trusted.** Two providers that both vouch for the same address would both reach the same person. The threat model says so: the provider must serve a single organisation.
- **Nothing limits failed attempts.** A bad session token or a bad agent token costs a hash and a database lookup each time, without limit.

## What changes

### 1. A person is bound to one identity

- **Two new columns on `persons`:** `issuer` and `subject`, both empty until the person first signs in, and then set together, once, and never changed. They are unique per tenant among persons who are not removed.
- **Sessions keep the identity too:** `issuer` and `subject`, from the verified ID token, beside the email they have now.
- **How a call finds its person in a tenant:**
  1. the person bound to the session's issuer and subject; or else
  2. the person with the session's email who is not yet bound, who is then bound to this issuer and subject, in the same transaction, with a `person.bound` event; or else
  3. nobody: the call is refused. That includes a person with this email who is bound to another identity. The refusal says the address belongs to another sign-in here and an owner can re-add the person.
- **The binding is per tenant.** One sign-in may be a person in several tenants, each bound separately. A later sign-in per organisation (a second issuer) then needs no further migration.
- **The tenant picker** lists the same tenants as step 1 or 2 would reach, without binding anything until a call is made in a tenant.
- **The database holds it.** Only the system sets `issuer` and `subject`, both at once, only while both are empty, and nothing changes them after. A test bypasses the Go code.
- **Undoing a binding** is what an owner already can do: remove the person and add them again by email. The new row is unbound, and the next sign-in binds it. No new screen is needed.

### 2. Dev login stays by email

- Dev login has no provider, so its sessions have no issuer or subject. It finds people by email, bound or not, as now, and binds nobody.
- That is no weaker than today. Dev login already lets anyone on the machine sign in as any email, and it answers only on loopback.

### 3. Failed attempts are limited

- **What counts as a failure:** a session or agent token that is unknown, expired or revoked, and an OIDC callback that fails (bad state, refused exchange, invalid ID token, unverified email).
- **Counted per client address:** in each server instance's memory, like the per-second call limit. Twenty failures a minute from one address are allowed; past that, every sign-in and token check from it is answered 429, or `resource_exhausted` with the seconds to wait, until the minute is over. A valid token from that address is refused too while it is limited, so the limit cannot be used to learn which tokens are valid.
- **The client address** is the connection's remote address. Behind a reverse proxy every client would share the proxy's address, so `MANDATE_CLIENT_IP_HEADER` (for example `X-Forwarded-For`) names a header to read it from instead. It is set only when the proxy is trusted to write that header. Without it, a server behind a proxy limits everyone together, and the README says so.
- **Why per instance and not in the database:** the tokens are 32 random bytes, so guessing is not the risk. Load is: each failure costs a lookup. A limit kept in memory costs nothing per call. Several instances each allow the full rate, as the threat model already says of the call limit.

## What does not change

- An email must still be verified by the provider before it binds or reaches anyone.
- Sessions last as long as they do now, and removing a person still ends their access in that tenant on their next call.
- The single-use sign-in link for the first owner (roadmap item 1) is designed with the quickstart. It must fit this model: a link session names its person directly and binds nothing.

## Schema

- `persons`: `issuer text`, `subject text`, a check that both are empty or both set, a unique index on `(tenant_id, issuer, subject)` for persons who are not removed, and the guard rule above.
- `sessions`: `issuer text`, `subject text`, both empty for dev login.
- `tenants_for_email` becomes `tenants_for_sign_in(email, issuer, subject)`, which says for each person whether they are bound to this identity, unbound, or bound to another. The auth service binds the person, as the system, in the tenant's scope, with its event in the same transaction.
- The first migration is edited in place, as before 0.1.0.

## Tests

- A person binds at first sign-in, and the same email under another subject is then refused in that tenant. In a tenant where the person is still unbound, the other subject binds instead.
- A changed email at the provider still reaches the bound person.
- One sign-in in two tenants binds in each.
- Removing and re-adding a person lets a new identity bind.
- Dev login reaches bound and unbound people by email, and binds nobody.
- The database refuses a binding by anyone but the system, a second binding, a change, and only one of the two columns set; these tests bypass the Go code.
- Twenty-one failures from one address are answered 429, a valid token from it too until the minute ends, and another address is unaffected. The header is read only when it is configured.
- The tenant picker lists the same tenants a call would reach.

## Questions for review

1. **Dev login by email only, binding nobody** (above). The other choice is to bind dev-login people to a made-up issuer. That would make a local tenant unusable with a real provider later.
2. **Twenty failures a minute per address,** and the header opt-in for proxies.
3. **No unbind screen:** removing and re-adding the person is the way, and it is logged.
