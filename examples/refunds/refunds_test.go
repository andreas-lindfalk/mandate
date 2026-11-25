package refunds_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/examples/refunds"
	"github.com/andreas-lindfalk/pgmandate/sign"
)

var (
	now      = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	tenantId = uuid.MustParse("7d0c0000-0000-0000-0000-000000000001")
	audience = "pgm:7d0c0000-0000-0000-0000-000000000001:refunds.issue:0123456789abcdef"
	signer   = mustSigner()
)

func mustSigner() *sign.Signer {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(40 + i)
	}
	s, err := sign.NewSigner(seed)
	if err != nil {
		panic(err)
	}
	return s
}

func receiver(store *refunds.Store) *httptest.Server {
	return httptest.NewServer(store.Receiver(sign.NewKeySet(signer.PublicKey()), audience, tenantId))
}

// deliver posts a signed delivery of a refund, as pgmandate does, and
// returns the status and the answer.
func deliver(t *testing.T, server *httptest.Server, deliveryId, proposalId uuid.UUID, payload map[string]any) (int, map[string]any) {
	return send(t, server, deliveryId, proposalId, now, time.Now().Add(time.Hour), audience, payload)
}

func send(t *testing.T, server *httptest.Server, deliveryId, proposalId uuid.UUID, proposedAt, notAfter time.Time, to string, payload map[string]any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"version": 2, "kind": "delivery", "audience": to, "tenantId": tenantId, "deliveryId": deliveryId,
		"correlationId": proposalId.String(), "createdAt": now, "notAfter": notAfter.UTC().Format(time.RFC3339),
		"proposal": map[string]any{"id": proposalId, "action": refunds.Action, "title": "Refund", "payload": payload, "risk": "medium", "agentId": uuid.New(), "proposedAt": proposedAt},
		"decision": map[string]any{"status": "executed", "decidedAt": now, "edited": false},
	})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(body))
	require.NoError(t, err)
	timestamp := time.Now().Unix()
	request.Header.Set(sign.HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	request.Header.Set(sign.HeaderSignature, signer.Sign(timestamp, body))
	request.Header.Set(sign.HeaderDeliveryId, deliveryId.String())
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	return response.StatusCode, answer
}

// refusedFor is the reason of a refusal, or "" when the answer is not one.
func refusedFor(answer map[string]any) string {
	if answer["outcome"] != "refused" {
		return ""
	}
	reason, _ := answer["refusal"].(map[string]any)["reason"].(string)
	return reason
}

func result(answer map[string]any) map[string]any {
	if answer["outcome"] != "done" {
		return nil
	}
	r, _ := answer["result"].(map[string]any)
	return r
}

func TestTheReceiverChecksEveryClaim(t *testing.T) {
	store := refunds.NewStore(refunds.Order{Id: "A-1", PaidCents: 12_000, Currency: "EUR", DeliveredAt: now.Add(-3 * 24 * time.Hour)})
	server := receiver(store)
	defer server.Close()
	claim := func(change func(map[string]any)) map[string]any {
		p := map[string]any{"orderId": "A-1", "amountCents": 4_000, "currency": "EUR", "daysSinceDelivery": 3}
		if change != nil {
			change(p)
		}
		return p
	}

	for name, c := range map[string]struct {
		change func(map[string]any)
		why    string
	}{
		"another order":    {func(p map[string]any) { p["orderId"] = "B-9" }, "no such order"},
		"another currency": {func(p map[string]any) { p["currency"] = "SEK" }, "the currency is not the order's"},
		"a fresher order":  {func(p map[string]any) { p["daysSinceDelivery"] = 1 }, "delivered longer ago than claimed"},
		"more than paid":   {func(p map[string]any) { p["amountCents"] = 12_001 }, "more than is left to refund"},
		"a negative sum":   {func(p map[string]any) { p["amountCents"] = -500 }, "more than zero"},
	} {
		status, answer := deliver(t, server, uuid.New(), uuid.New(), claim(c.change))
		require.Equal(t, http.StatusUnprocessableEntity, status, name)
		require.Contains(t, refusedFor(answer), c.why, name)
		for _, record := range []string{"12000", "EUR", "3 days", "A-1"} {
			require.NotContains(t, refusedFor(answer), record, "a refusal never tells what the records hold: "+name)
		}
	}
	order, _ := store.Order("A-1")
	require.Zero(t, order.RefundedCents, "a refused claim moves nothing")

	// A true claim is carried out, once per proposal, however often it
	// arrives: as the same delivery again, or as a new delivery.
	proposal, delivery := uuid.New(), uuid.New()
	status, answer := deliver(t, server, delivery, proposal, claim(nil))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "RF-"+proposal.String()[:8], result(answer)["refundId"])
	status, again := deliver(t, server, delivery, proposal, claim(nil))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, answer, again)
	status, _ = deliver(t, server, uuid.New(), proposal, claim(nil))
	require.Equal(t, http.StatusOK, status)
	order, _ = store.Order("A-1")
	require.Equal(t, int64(4_000), order.RefundedCents)

	// What is left bounds the next refund.
	status, answer = deliver(t, server, uuid.New(), uuid.New(), claim(func(p map[string]any) { p["amountCents"] = 8_001 }))
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Contains(t, refusedFor(answer), "more than is left to refund")

	// A claim that makes the case look weaker than the records do stands.
	status, _ = deliver(t, server, uuid.New(), uuid.New(), claim(func(p map[string]any) { p["daysSinceDelivery"] = 10; p["amountCents"] = 100 }))
	require.Equal(t, http.StatusOK, status)
}

// A claim is true as of when the agent made it: a refund a person approves
// days later, or a delivery retried tomorrow, still matches.
func TestAClaimIsCheckedAsOfTheProposal(t *testing.T) {
	store := refunds.NewStore(refunds.Order{Id: "A-1", PaidCents: 12_000, Currency: "EUR", DeliveredAt: now.Add(-4 * 24 * time.Hour)})
	server := receiver(store)
	defer server.Close()
	proposedYesterday := now.Add(-24 * time.Hour)
	status, answer := send(t, server, uuid.New(), uuid.New(), proposedYesterday, time.Now().Add(time.Hour), audience,
		map[string]any{"orderId": "A-1", "amountCents": 100, "currency": "EUR", "daysSinceDelivery": 3})
	require.Equal(t, http.StatusOK, status, answer)
	require.Equal(t, "done", answer["outcome"])
}

// A delivery past its notAfter is refused, one for another audience is
// answered without an outcome, and neither moves anything.
func TestTooLateOrNotOursMovesNothing(t *testing.T) {
	store := refunds.NewStore(refunds.Order{Id: "A-1", PaidCents: 12_000, Currency: "EUR", DeliveredAt: now.Add(-3 * 24 * time.Hour)})
	server := receiver(store)
	defer server.Close()
	claim := map[string]any{"orderId": "A-1", "amountCents": 100, "currency": "EUR", "daysSinceDelivery": 3}

	_, answer := send(t, server, uuid.New(), uuid.New(), now, time.Now().Add(-time.Second), audience, claim)
	require.Equal(t, "too_late", refusedFor(answer))
	status, answer := send(t, server, uuid.New(), uuid.New(), now, time.Now().Add(time.Hour), audience[:len(audience)-1]+"0", claim)
	require.Equal(t, http.StatusForbidden, status)
	require.Nil(t, answer["outcome"], "retried, not refused: the receiver may not know a new audience yet")
	require.Equal(t, "not_for_this_receiver", answer["error"])
	order, _ := store.Order("A-1")
	require.Zero(t, order.RefundedCents)
}
