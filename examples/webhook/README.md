# webhook: a reference receiver

An `http.Handler` to mount at the URL of a webhook action. It receives decided proposals under delivery contract v2 and answers each with an explicit outcome. It imports only pgmandate's `sign` package, so it is also the shape to follow in another language. [docs/receivers.md](../../docs/receivers.md) is the contract, step by step.

## Try it

`make demo` uses it as its stand-in helpdesk. After the demo, the command it prints starts it on its own, so that what you approve in the UI has somewhere to land:

```sh
go run ./cmd/pgmandate receiver -tenant <tenant id> -audience <audience>
```

It fetches the keys it trusts from the running server's `/.well-known/pgmandate-keys`. The tests run without a server: `go test ./examples/webhook/`.

## Reading it

1. The package comment in [webhook.go](webhook.go): the five things a receiver does, in order.
2. `ServeHTTP`: the same five, in code. Verify the signature over the raw body; check the kind, audience and tenant; claim the delivery id; refuse what comes after `notAfter`; call `Handle` and answer.
3. `Handle` and the errors `ErrRefused` and `ErrUnknown`: how your code says *done*, *refused* or *unknown*. Anything else is answered 500 and retried, so `Handle` must have done nothing then, or be safe to run twice.
4. [webhook_test.go](webhook_test.go): a panic, final outcomes, a copy still in progress, the envelope checked first, and a rotated key.

## Before you copy it

- **It keeps the delivery ids it claimed in memory.** A restart forgets them, and a delivery retried after it would be carried out again. A real receiver keeps them where a restart cannot lose them, at least until `notAfter` plus five minutes, or in the same transaction as its work.
- `FailNext` and `Received` are there for the demo and the tests.
- It answers for the audiences it was told to `Trust`, and never for one taken from the request.
