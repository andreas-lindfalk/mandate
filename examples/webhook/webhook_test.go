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
	id       uuid.UUID
	audience string
	tenant   uuid.UUID
	kind     string
	notAfter time.Time
	by       *sign.Signer
}

func fresh() delivery {
	return delivery{id: uuid.New(), audience: audience, tenant: tenantId, kind: "delivery", notAfter: time.Now().Add(time.Hour), by: signer}
}

func post(t *testing.T, url string, d delivery) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"version": 2, "kind": d.kind, "audience": d.audience, "tenantId": d.tenant, "deliveryId": d.id,
		"notAfter": d.notAfter.UTC().Format(time.RFC3339),
		"proposal": map[string]any{"id": uuid.New(), "action": "a.b", "payload": map[string]any{}},
		"decision": map[string]any{"status": "approved"},
	})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	timestamp := time.Now().Unix()
	request.Header.Set(sign.HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	request.Header.Set(sign.HeaderSignature, d.by.Sign(timestamp, body))
	request.Header.Set(sign.HeaderDeliveryId, d.id.String())
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(response.Body)
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	return response.StatusCode, answer
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

// What is not this receiver's is answered without an outcome, so it is
// retried (an action just moved here may not be configured yet); what
// comes too late is refused; a signature that does not hold is answered
// 401. Nothing runs for any of them.
func TestTheEnvelopeIsCheckedFirst(t *testing.T) {
	receiver := newReceiver()
	ran := false
	receiver.Handle = func(webhook.Received) (any, error) { ran = true; return nil, nil }
	server := httptest.NewServer(receiver)
	defer server.Close()

	for name, change := range map[string]func(*delivery){
		"another audience": func(d *delivery) { d.audience = audience[:len(audience)-1] + "0" },
		"another tenant":   func(d *delivery) { d.tenant = uuid.New() },
		"a callback":       func(d *delivery) { d.kind = "callback" },
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
