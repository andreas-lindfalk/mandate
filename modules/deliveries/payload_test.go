package deliveries

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
)

func TestPayloadOf(t *testing.T) {
	tenantId, decidedBy := uuid.New(), uuid.New()
	decidedAt := "2026-11-24T10:00:05Z"
	delivery := Delivery{Id: uuid.New(), Audience: "pgm:x", NotAfter: time.Date(2026, 11, 24, 11, 0, 5, 0, time.UTC), CreatedAt: time.Date(2026, 11, 24, 11, 0, 5, 999, time.FixedZone("CET", 3600))}
	proposal := proposals.Proposal{
		Id: uuid.New(), AgentId: uuid.New(), Action: "tickets.create", Risk: "low", Title: "Printer on floor 3", Payload: json.RawMessage(`{"subject":"Printer on 3"}`),
		ProposedPayload: json.RawMessage(`{"subject":"Printer"}`), Edited: true, DecisionNote: "checked",
		Status: proposals.StatusApproved, DecidedBy: &decidedBy, DecidedAt: &decidedAt, CreatedAt: "2026-11-24T10:00:00Z",
	}

	payload, err := payloadOf(tenantId, delivery, proposal)
	require.NoError(t, err)
	require.Equal(t, 2, payload.Version)
	require.Equal(t, "delivery", payload.Kind)
	require.Equal(t, "pgm:x", payload.Audience)
	require.Equal(t, "2026-11-24T11:00:05Z", payload.NotAfter)
	require.Equal(t, proposal.Id.String(), payload.CorrelationId, "without a run, the proposal's id")
	require.Empty(t, payload.Proposal.RunId)
	require.Equal(t, "2026-11-24T10:00:05Z", payload.CreatedAt, "in UTC, to the second")
	require.Equal(t, "2026-11-24T10:00:00Z", payload.Proposal.ProposedAt)
	require.Equal(t, decidedAt, payload.Decision.DecidedAt)
	// A person's edit is what the receiver carries out, not the agent's proposal.
	require.JSONEq(t, `{"subject":"Printer on 3"}`, string(payload.Proposal.Payload), "the payload as approved")
	require.Equal(t, "Printer on floor 3", payload.Proposal.Title)
	require.True(t, payload.Decision.Edited)
	require.Equal(t, "checked", payload.Decision.Note)

	proposal.RunId = "run-7"
	payload, err = payloadOf(tenantId, delivery, proposal)
	require.NoError(t, err)
	require.Equal(t, "run-7", payload.CorrelationId)
	require.Equal(t, "run-7", payload.Proposal.RunId)

	// A proposal with no decision has no body the contract describes.
	proposal.DecidedAt = nil
	_, err = payloadOf(tenantId, delivery, proposal)
	require.Error(t, err)
}
