package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/examples/webhook"
	"github.com/andreas-lindfalk/pgmandate/sign"
)

var (
	tenantId = uuid.MustParse("7d0c0000-0000-0000-0000-000000000001")
	audience = "pgm:7d0c0000-0000-0000-0000-000000000001:a.b:0123456789abcdef"
	signer   = newSigner(1)
)

func newSigner(fill byte) *sign.Signer {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = fill + byte(i)
	}
	s, err := sign.NewSigner(seed)
	if err != nil {
		panic(err)
	}
	return s
}

func newReceiver() *webhook.Receiver {
	r := webhook.NewReceiver(sign.NewKeySet(signer.PublicKey()))
	r.Trust(audience, tenantId)
	return r
}

type delivery struct {
	version  int
	id       uuid.UUID
	audience string
	tenant   uuid.UUID
	kind     string
	// status is the decision's status, "approved" from fresh.
	status   string
	notAfter time.Time
	by       *sign.Signer
	// signedAt is when it was signed; zero is now.
	signedAt time.Time
}

func fresh() delivery {
	return delivery{version: 2, id: uuid.New(), audience: audience, tenant: tenantId, kind: "delivery", status: "approved", notAfter: time.Now().Add(time.Hour), by: signer}
}

func post(t *testing.T, url string, d delivery) (int, map[string]any) {
	t.Helper()
	response, err := http.DefaultClient.Do(newRequest(t, context.Background(), url, d))
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(response.Body)
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	return response.StatusCode, answer
}

// newRequest is the signed request that delivers d.
func newRequest(t *testing.T, ctx context.Context, url string, d delivery) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"version": d.version, "kind": d.kind, "audience": d.audience, "tenantId": d.tenant, "deliveryId": d.id,
		"notAfter": d.notAfter.UTC().Format(time.RFC3339),
		"proposal": map[string]any{"id": uuid.New(), "action": "a.b", "payload": map[string]any{}},
		"decision": map[string]any{"status": d.status},
	})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	timestamp := time.Now().Unix()
	if !d.signedAt.IsZero() {
		timestamp = d.signedAt.Unix()
	}
	request.Header.Set(sign.HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	request.Header.Set(sign.HeaderSignature, d.by.Sign(timestamp, body))
	request.Header.Set(sign.HeaderDeliveryId, d.id.String())
	return request
}

func reason(answer map[string]any) any {
	refusal, _ := answer["refusal"].(map[string]any)
	return refusal["reason"]
}

// A delivery whose handling panicked is not taken as done: the retry runs
// it again. After that it is answered, never run again.
func TestHandleRunsAgainAfterAPanic(t *testing.T) {
	receiver := newReceiver()
	calls := 0
	receiver.Handle = func(webhook.Received) (any, error) {
		calls++
		if calls == 1 {
			panic("the database went away")
		}
		return map[string]any{"done": true}, nil
	}
	server := httptest.NewServer(receiver)
	defer server.Close()
	d := fresh()
	status, _ := post(t, server.URL, d)
	require.Equal(t, http.StatusInternalServerError, status)
	status, answer := post(t, server.URL, d)
	require.Equal(t, http.StatusOK, status, "the retry runs it again")
	require.Equal(t, "done", answer["outcome"])
	_, again := post(t, server.URL, d)
	require.Equal(t, answer, again, "and after that it is answered the same, not run")
	require.Equal(t, 2, calls)
	require.Len(t, receiver.Received(), 1, "a delivery that ran again is recorded once")

	// Any other error is not an outcome: answered 500, and retried.
	receiver.Handle = func(webhook.Received) (any, error) { return nil, errors.New("the database went away") }
	status, _ = post(t, server.URL, fresh())
	require.Equal(t, http.StatusInternalServerError, status)
}

// A refusal and an unknown outcome are final, and answered the same way
// again: the receiver never runs a delivery twice.
func TestOutcomesAreFinal(t *testing.T) {
	receiver := newReceiver()
	calls := 0
	receiver.Handle = func(r webhook.Received) (any, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("%w: no such order", webhook.ErrRefused)
		}
		return nil, fmt.Errorf("%w: the bank did not answer", webhook.ErrUnknown)
	}
	server := httptest.NewServer(receiver)
	defer server.Close()
	refused := fresh()
	status, answer := post(t, server.URL, refused)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "refused", answer["outcome"])
	require.Contains(t, reason(answer), "no such order")
	_, again := post(t, server.URL, refused)
	require.Equal(t, answer, again)

	unknown := fresh()
	status, answer = post(t, server.URL, unknown)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "unknown", answer["outcome"])
	_, again = post(t, server.URL, unknown)
	require.Equal(t, answer, again)
	require.Equal(t, 2, calls)
}

// A copy that arrives while the first is still being carried out is asked
// to come back later, never run beside it.
func TestACopyInProgressIsRetried(t *testing.T) {
	receiver := newReceiver()
	started, release := make(chan struct{}), make(chan struct{})
	calls := 0
	receiver.Handle = func(webhook.Received) (any, error) {
		calls++
		close(started)
		<-release
		return map[string]any{"done": true}, nil
	}
	server := httptest.NewServer(receiver)
	defer server.Close()
	d := fresh()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		status, _ := post(t, server.URL, d)
		require.Equal(t, http.StatusOK, status)
	}()
	<-started
	status, _ := post(t, server.URL, d)
	require.Equal(t, http.StatusServiceUnavailable, status)
	close(release)
	wg.Wait()
	require.Equal(t, 1, calls)
}

// What is not this receiver's, a version it does not know among them, is
// answered without an outcome, so it is retried (an action just moved here
// may not be configured yet); what comes too late is refused; a signature
// that does not hold is answered 401. Nothing runs for any of them.
func TestTheEnvelopeIsCheckedFirst(t *testing.T) {
	receiver := newReceiver()
	ran := false
	receiver.Handle = func(webhook.Received) (any, error) { ran = true; return nil, nil }
	server := httptest.NewServer(receiver)
	defer server.Close()

	for name, change := range map[string]func(*delivery){
		"another audience":   func(d *delivery) { d.audience = audience[:len(audience)-1] + "0" },
		"another tenant":     func(d *delivery) { d.tenant = uuid.New() },
		"a callback":         func(d *delivery) { d.kind = "callback" },
		"an unknown version": func(d *delivery) { d.version = 3 },
	} {
		d := fresh()
		change(&d)
		status, answer := post(t, server.URL, d)
		require.Equal(t, http.StatusForbidden, status, name)
		require.Nil(t, answer["outcome"], "%s: no outcome, so pgmandate tries again", name)
		require.Equal(t, "not_for_this_receiver", answer["error"], name)
	}
	late := fresh()
	late.notAfter = time.Now().Add(-time.Second)
	_, answer := post(t, server.URL, late)
	require.Equal(t, "too_late", reason(answer))

	forged := fresh()
	forged.by = newSigner(9)
	status, _ := post(t, server.URL, forged)
	require.Equal(t, http.StatusUnauthorized, status)
	require.False(t, ran)
}

// Only an approved or executed decision is carried out. A delivery whose
// decision says anything else is refused, and nothing runs or is recorded.
func TestOnlyADecisionIsCarriedOut(t *testing.T) {
	receiver := newReceiver()
	ran := false
	receiver.Handle = func(webhook.Received) (any, error) { ran = true; return map[string]any{"done": true}, nil }
	server := httptest.NewServer(receiver)
	defer server.Close()

	for _, status := range []string{"rejected", "pending", "withdrawn", ""} {
		d := fresh()
		d.status = status
		code, answer := post(t, server.URL, d)
		require.Equal(t, http.StatusForbidden, code, "%q", status)
		require.Equal(t, "refused", answer["outcome"], "%q", status)
		require.Equal(t, "not_a_decision", reason(answer), "%q", status)
	}
	require.False(t, ran, "nothing that is not a decision runs")
	require.Empty(t, receiver.Received())

	executed := fresh()
	executed.status = "executed"
	code, answer := post(t, server.URL, executed)
	require.Equal(t, http.StatusOK, code, answer)
	require.Equal(t, "done", answer["outcome"])
	require.True(t, ran)
	require.Len(t, receiver.Received(), 1)
}

// After a rotation the receiver meets a key id it does not know, fetches
// the published set again, and verifies with it. It fetches at most once a
// minute, and the set it fetches replaces the keys it had.
func TestARotatedKeyIsFetched(t *testing.T) {
	rotated := newSigner(5)
	fetches := 0
	receiver := newReceiver()
	receiver.Refresh = func(context.Context) (sign.KeySet, error) {
		fetches++
		return sign.NewKeySet(rotated.PublicKey()), nil
	}
	server := httptest.NewServer(receiver)
	defer server.Close()

	status, _ := post(t, server.URL, fresh())
	require.Equal(t, http.StatusOK, status)
	require.Zero(t, fetches, "a known key needs no fetch")

	byRotated := fresh()
	byRotated.by = rotated
	status, answer := post(t, server.URL, byRotated)
	require.Equal(t, http.StatusOK, status, answer)
	require.Equal(t, "done", answer["outcome"])
	require.Equal(t, 1, fetches)

	unknown := fresh()
	unknown.by = newSigner(9)
	status, _ = post(t, server.URL, unknown)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Equal(t, 1, fetches, "not fetched again within the minute")
	status, _ = post(t, server.URL, fresh())
	require.Equal(t, http.StatusUnauthorized, status, "the fetched set replaced the old key")

	// A fetch that fails leaves the keys as they were.
	failing := newReceiver()
	failing.Refresh = func(context.Context) (sign.KeySet, error) { return nil, errors.New("down") }
	failingServer := httptest.NewServer(failing)
	defer failingServer.Close()
	status, _ = post(t, failingServer.URL, byRotated)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = post(t, failingServer.URL, fresh())
	require.Equal(t, http.StatusOK, status)
}

// A key dropped from the published set stops verifying once the keys the
// receiver holds are five minutes old, though every delivery since the
// rotation was signed with a key it knows: it fetches the set again then,
// not only on a key id it does not know.
func TestADroppedKeyIsRevoked(t *testing.T) {
	old, current := signer, newSigner(5)
	var mu sync.Mutex
	published, fetches := sign.NewKeySet(current.PublicKey(), old.PublicKey()), 0
	now := time.Now()
	receiver := newReceiver()
	webhook.SetClock(receiver, func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	receiver.Refresh = func(context.Context) (sign.KeySet, error) {
		mu.Lock()
		defer mu.Unlock()
		fetches++
		return published, nil
	}
	server := httptest.NewServer(receiver)
	defer server.Close()
	by := func(s *sign.Signer) delivery {
		d := fresh()
		d.by = s
		mu.Lock()
		defer mu.Unlock()
		d.signedAt = now
		return d
	}

	// The rotation: the new key is fetched with the old one still in the set.
	status, _ := post(t, server.URL, by(current))
	require.Equal(t, http.StatusOK, status)
	status, _ = post(t, server.URL, by(old))
	require.Equal(t, http.StatusOK, status, "the old key is trusted while it is published")
	require.Equal(t, 1, fetches)

	// The old key is dropped. Within five minutes of the fetch it is still
	// trusted, as the published max-age allows.
	mu.Lock()
	published, now = sign.NewKeySet(current.PublicKey()), now.Add(5*time.Minute-time.Second)
	mu.Unlock()
	status, _ = post(t, server.URL, by(old))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 1, fetches, "keys younger than five minutes are not fetched again")

	// After that the set is fetched again before verifying, and the dropped
	// key is refused.
	mu.Lock()
	now = now.Add(time.Second)
	mu.Unlock()
	status, _ = post(t, server.URL, by(old))
	require.Equal(t, http.StatusUnauthorized, status, "a dropped key is revoked")
	require.Equal(t, 2, fetches)
	status, _ = post(t, server.URL, by(current))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 2, fetches, "the fetched set is used for five minutes again")
}

// A client that closes its connection while the receiver fetches stale keys
// does not cut the fetch short: it runs on, with a deadline of its own, and
// a key dropped from the set is refused right after, also by a delivery
// that arrived while the fetch was under way.
func TestAnAbortedRequestDoesNotKeepADroppedKey(t *testing.T) {
	old, current := signer, newSigner(5)
	now := time.Now().Add(webhook.KeysMaxAge + time.Second)
	receiver := newReceiver()
	webhook.SetClock(receiver, func() time.Time { return now })
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	fetches, deadlines := 0, 0
	receiver.Refresh = func(ctx context.Context) (sign.KeySet, error) {
		mu.Lock()
		fetches++
		if _, ok := ctx.Deadline(); ok {
			deadlines++
		}
		mu.Unlock()
		close(started)
		<-release
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return sign.NewKeySet(current.PublicKey()), nil
	}
	gone := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		go func() { <-req.Context().Done(); once.Do(func() { close(gone) }) }()
		receiver.ServeHTTP(w, req)
	}))
	defer server.Close()
	by := func(s *sign.Signer) delivery { d := fresh(); d.by = s; d.signedAt = now; return d }

	// Keys five minutes old: the first delivery starts a fetch, and its
	// client closes the connection while the fetch is under way.
	ctx, cancel := context.WithCancel(context.Background())
	aborted := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(newRequest(t, ctx, server.URL, by(old)))
		if err == nil {
			_ = response.Body.Close()
		}
		aborted <- err
	}()
	<-started
	cancel()
	require.Error(t, <-aborted)
	<-gone

	// A delivery that arrives meanwhile waits for that fetch.
	waiting := make(chan int, 1)
	go func() {
		status, _ := post(t, server.URL, by(old))
		waiting <- status
	}()
	close(release)
	require.Equal(t, http.StatusUnauthorized, <-waiting, "the dropped key is refused")
	status, _ := post(t, server.URL, by(old))
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = post(t, server.URL, by(current))
	require.Equal(t, http.StatusOK, status)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, fetches)
	require.Equal(t, 1, deadlines, "the fetch is bounded")
}

// Keys five minutes old that cannot be fetched again verify nothing: the
// delivery is answered 503 without an outcome, and retried, until a fetch
// succeeds, at most once a minute.
func TestStaleKeysThatCannotBeFetchedVerifyNothing(t *testing.T) {
	old, current := signer, newSigner(5)
	var mu sync.Mutex
	now, fetches := time.Now().Add(webhook.KeysMaxAge+time.Second), 0
	var published sign.KeySet
	receiver := newReceiver()
	webhook.SetClock(receiver, func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	receiver.Refresh = func(context.Context) (sign.KeySet, error) {
		mu.Lock()
		defer mu.Unlock()
		fetches++
		if published == nil {
			return nil, errors.New("down")
		}
		return published, nil
	}
	ran := false
	receiver.Handle = func(webhook.Received) (any, error) { ran = true; return nil, nil }
	server := httptest.NewServer(receiver)
	defer server.Close()
	by := func(s *sign.Signer) delivery {
		d := fresh()
		d.by = s
		mu.Lock()
		defer mu.Unlock()
		d.signedAt = now
		return d
	}

	status, answer := post(t, server.URL, by(old))
	require.Equal(t, http.StatusServiceUnavailable, status, "the old key is not trusted on keys that may be stale")
	require.Nil(t, answer["outcome"])
	status, _ = post(t, server.URL, by(old))
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.False(t, ran)
	mu.Lock()
	require.Equal(t, 1, fetches, "not fetched again within the minute")
	published, now = sign.NewKeySet(current.PublicKey()), now.Add(time.Minute)
	mu.Unlock()

	status, _ = post(t, server.URL, by(old))
	require.Equal(t, http.StatusUnauthorized, status, "the fetched set dropped it")
	status, _ = post(t, server.URL, by(current))
	require.Equal(t, http.StatusOK, status)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, fetches)
}
