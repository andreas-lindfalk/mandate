# refunds: check the claims, then pay once

A receiver for `refunds.issue`, built on [webhook](../webhook). It shows what every receiver of an action that moves value must do: the payload is what the agent says, so the receiver checks each fact in it against its own records before it acts.

A mandate's condition such as `payload.amountCents <= 5000 && payload.daysSinceDelivery <= 14` decides on the agent's claim, and Postgres checks the condition on that same claim. Neither knows whether the claim is true. Only the system that holds the order does.

## Try it

```sh
go test ./examples/refunds/
```

The tests sign deliveries themselves, as pgmandate does, and need no server. To use it with one, define the action with `refunds.Schema`, mount `Store.Receiver` at its URL, and give it the action's audience and the server's keys.

## Reading it

1. The package comment in [refunds.go](refunds.go): why a claim is not a fact.
2. `Schema` and `Request`: the payload, and which of its fields are claims.
3. `Apply`, top to bottom:
   - a proposal already paid is answered as before;
   - nothing is paid once `notAfter` has passed, checked just before the write;
   - then the order, its currency, the days since delivery, as of when the agent proposed, and what is left to refund.

   A mismatch answers *refused*, which is final: nothing happens, the delivery is not retried, and a person sees why.
4. `ErrMismatch`: the refusal names the claim that failed, never what the records hold, so an agent cannot read the records by trying claims.
5. [refunds_test.go](refunds_test.go): every claim checked, claims checked as of the proposal, and nothing moved when it is too late or not for this receiver.

## Before you copy it

- **`Store` stands in for your own system.** Its orders and refunds live in memory. In yours, the check and the write belong in one transaction, keyed by the proposal or delivery id, so a retry finds the refund already made.
- It inherits the webhook example's memory of delivery ids, with the same warning.
