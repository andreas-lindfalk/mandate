package webhook_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/examples/webhook"
	"github.com/andreas-lindfalk/pgmandate/sign"
)

const secret = "mws_webhook_test_secret_0123456789"

func post(t *testing.T, url string, deliveryId uuid.UUID) int {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"version": 1, "deliveryId": deliveryId, "tenantId": uuid.New(),
		"proposal": map[string]any{"id": uuid.New(), "action": "a.b", "payload": map[string]any{}},
		"decision": map[string]any{"status": "approved"},
	})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	timestamp := time.Now().Unix()
	request.Header.Set(sign.HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	request.Header.Set(sign.HeaderSignature, sign.Signature([]byte(secret), timestamp, body))
	request.Header.Set(sign.HeaderDeliveryId, deliveryId.String())
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	_ = response.Body.Close()
	return response.StatusCode
}

// A delivery whose handling panicked is not taken as done: the retry runs
// it again. One that was refused is answered the same way again.
func TestHandleRunsAgainAfterAPanic(t *testing.T) {
	receiver := webhook.NewReceiver(secret)
	calls := 0
	receiver.Handle = func(webhook.Received) (any, error) {
		calls++
		switch calls {
		case 1:
			panic("the database went away")
		case 2:
			return map[string]any{"done": true}, nil
		}
		return nil, errors.New("never twice")
	}
	server := httptest.NewServer(receiver)
	defer server.Close()
	id := uuid.New()
	require.Equal(t, http.StatusInternalServerError, post(t, server.URL, id))
	require.Equal(t, http.StatusOK, post(t, server.URL, id), "the retry runs it again")
	require.Equal(t, http.StatusOK, post(t, server.URL, id), "and after that it is answered, not run")
	require.Equal(t, 2, calls)

	// A refusal is final for pgmandate, but not remembered here: once the
	// records are fixed, a person sends it again and it runs again.
	fixed := false
	refusals := 0
	receiver.Handle = func(webhook.Received) (any, error) {
		if !fixed {
			refusals++
			return nil, fmt.Errorf("%w: no such order", webhook.ErrRefused)
		}
		return map[string]any{"done": true}, nil
	}
	refused := uuid.New()
	require.Equal(t, http.StatusUnprocessableEntity, post(t, server.URL, refused))
	require.Equal(t, http.StatusUnprocessableEntity, post(t, server.URL, refused))
	require.Equal(t, 2, refusals)
	fixed = true
	require.Equal(t, http.StatusOK, post(t, server.URL, refused))
	count := 0
	for _, r := range receiver.Received() {
		if r.DeliveryId == refused {
			count++
		}
	}
	require.Equal(t, 1, count, "a delivery that ran again is recorded once")

	// Any other error is not a refusal: answered 500, and pgmandate retries.
	receiver.Handle = func(webhook.Received) (any, error) { return nil, errors.New("the database went away") }
	require.Equal(t, http.StatusInternalServerError, post(t, server.URL, uuid.New()))
}
