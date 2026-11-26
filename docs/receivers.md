# Writing a receiver

The service next to your system that carries out a decided proposal. The front page is the [README](../README.md); runnable receivers are in [examples/](../examples/README.md).

A decided proposal is posted to its action's URL, under delivery contract v2:

```
POST <url>
Content-Type: application/json
User-Agent: pgmandate
Mandate-Delivery-Id: <uuid>
Mandate-Timestamp: <unix seconds, by the database's clock>
Mandate-Signature: v2 kid=<key id> sig=<standard base64 Ed25519 signature>

{ "version": 2, "kind": "delivery", "audience", "tenantId", "deliveryId", "correlationId", "createdAt", "notAfter",
  "proposal": { "id", "action", "title", "payload", "risk", "agentId", "runId"?, "proposedAt" },
  "decision": { "status": "approved" | "executed", "decidedAt", "edited", "decidedBy"?, "note"?, "executionBasis"? } }
```

The body is described field by field in [delivery-v2.schema.json](delivery-v2.schema.json), a JSON Schema to validate against or generate types from in any language. A test holds the server to it. A receiver is configured with two things, neither of them secret: the action's **audience**, which `action define` prints and the UI shows, with its tenant id; and the **keys** it trusts, fetched from `<server>/.well-known/pgmandate-keys` or given by hand. It does these in order:

1. **Verify the signature** over the raw bytes, before parsing anything. The signed bytes are `pgmandate-v2.`, the timestamp header's digits, a `.`, and the raw body as received; the signature is Ed25519 with the public key whose id is `kid`. Read the raw body, not a parsed one: `express.raw({ type: "application/json" })` in Express, `request.get_data()` in Flask. Refuse a key id you do not trust, and a timestamp more than five minutes from your clock. Answer 401 for a signature you cannot verify. In Go, `sign.Verify` does all of it, and `sign.Fetch` reads the published set.
2. **Check it is for you.** `version` is 2, `kind` is `delivery`, `audience` and `tenantId` are the ones you were configured with (never taken from the request), and `deliveryId` equals the header's. Otherwise do nothing and answer 403 without an outcome, so it is retried: an action just moved to you has a new audience you may not have been given yet. In Go, `sign.CheckEnvelope`.
3. **Claim the delivery id, atomically.** A delivery you finished before is answered exactly as then. One still in progress is answered 503, which is retried. Keep the ids where a restart cannot lose them, until at least `notAfter` plus five minutes.
4. **Refuse what comes too late.** A delivery you never started that arrives after `notAfter` by your clock is answered `{"outcome":"refused","refusal":{"reason":"too_late"}}`, and nothing is done.
5. **Check the facts and act, and never commit after `notAfter`.** Abort work still under way at `notAfter`; answer `refused` if you are sure nothing took effect, and `unknown` if you are not.
6. **Answer explicitly**, with a JSON object of at most 4 kB, within 15 seconds:

| Answer | What pgmandate records |
|---|---|
| 2xx `{"outcome":"done","result":{…}}` | done. The result is kept and shown to the agent, so return what it needs, such as the id of what you created |
| any status `{"outcome":"refused","refusal":{…}}` | refused: you did nothing, and that is final. A refusal is always this shape |
| 2xx `{"outcome":"unknown","reason":"…"}` | unknown: you cannot tell, as when your own call timed out. A person settles it |
| anything else: other bodies, redirects (never followed), other 4xx, 5xx, a timeout | nothing final: retried, up to eight attempts and never later than `notAfter` minus 15 seconds |

A proxy's 404 is never read as a refusal. When the attempts run out, a delivery that was possibly sent becomes *unknown*, and one that provably never reached you (it failed before its headers were written) becomes *not sent*. A person may send a delivery that was never sent again, with the same delivery id, until `notAfter` minus 15 seconds. A person settles an unknown one as done or not done, but only from `notAfter` plus 15 seconds plus five minutes, when you can no longer carry it out. After a refusal, or one settled as not done, the agent may propose the same thing again under a new key.

- **Check your verification against a known answer.** The seed `AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=` has the public key `A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=` and the key id `56475aa75463474c` (the first 16 hex digits of the SHA-256 of the public key). With the timestamp `1767225600` and the body `{"version":2,"kind":"delivery","deliveryId":"00000000-0000-0000-0000-000000000001"}` it gives `v2 kid=56475aa75463474c sig=RQk5h/yRhlTstIABLiW11fXXFg/5FmlDJtWnmdagCNPieC5C5ZCfUVsT9eXzRY9JlMdqEz1R0g3MLLC7gergCQ==`.
- **Keys and rotation.** The published set holds the key that signs and any previous ones still trusted. A fetched set replaces what you cached; it never adds to it, so dropping a key from the set revokes it. Fetch it over https from an address you were configured with. When the server starts signing with a new key, a receiver that has not fetched it answers 401 and the attempt is retried, so fetch the set again when you meet a key id you do not know (at most once a minute). Fetch it again, too, once what you hold is five minutes old (the `max-age` it is published with), or a key dropped from the set stays trusted for as long as every delivery is signed with a key you know. Until that fetch succeeds, answer 503 rather than verify with what you hold, and fetch with a timeout of your own, not the request's, so that a client that closes its connection cannot cut the fetch short and keep the old set in use.
- **The audience changes** when an action is defined, defined again after removal, or moved to a new URL, so an audience never repeats. Deliveries still waiting, and never possibly sent, move to the new URL and audience; the receiver at the old one gets nothing more, apart from an attempt already under way. A receiver that serves several actions trusts each audience.
- **Check the version.** A field can be added within a version, so ignore fields you do not know. A removed field or a changed meaning is a new version: answer a version you do not know as in step 2, 403 without an outcome, never a refusal.
- **Log the correlation id.** It is the run id the agent gave when it proposed, otherwise the proposal's id, so the agent's logs, pgmandate's log and yours can be joined on one value. A run id is the agent's own word: use it to group, and use `proposal.id` and `agentId` to identify.
- **Times** are RFC 3339 in UTC. The body is the same on every attempt for one audience; the time of an attempt is the `Mandate-Timestamp` header.
- **Act only on** `decision.status` `approved` or `executed`.
- **Plain http only on private addresses.** Unless the operator allows local and private connectors, a connector must be `https`.
- **No ordering.** Two deliveries from the same agent may arrive in either order.
- **The payload fits the action's schema** as the schema was when the decision was made. If the schema changed after the agent proposed and the payload no longer fits, an approval needs an edit that fits and a mandate does not allow it.
- Fields marked `?` are left out when empty: `runId` when the agent gave none, `decidedBy` for an execution, `executionBasis` for an approval.

[examples/webhook](../examples/webhook) is a reference handler to mount in your own server, and `pgmandate receiver` runs it locally.

**Check the claims before you act.** The payload is what the agent says, and a mandate's condition decides on what the agent says: `payload.daysSinceDelivery <= 14` is true when the agent wrote 3, whatever the order shows. For an action that moves value, read every fact the payload states from your own records, answer `refused` when one does not match (nothing happens, the delivery is not retried, and a person sees your answer), and carry it out once per proposal. [examples/refunds](../examples/refunds) does that for a refund.
