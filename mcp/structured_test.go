package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	agentv1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/agent/v1"
)

// A field of JSON text becomes the object under the name without "Json",
// keeping every digit, at any depth; anything else is left as it is.
func TestStructured(t *testing.T) {
	out, err := Structured(&agentv1.GetProposalResponse{
		Proposal: &agentv1.Proposal{
			ProposalId: "p", PayloadJson: `{"account":123456789012345678901234567890,"rate":0.12345678901234567891}`,
			RefusalJson: `{"reason":"no"}`, Flags: []string{"steering_text"},
		},
		History: []*agentv1.Event{{Seq: 3, Type: "proposal.created", PayloadJson: `{"payloadSha256":"ab"}`}},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"proposal": {"proposalId": "p", "payload": {"account": 123456789012345678901234567890, "rate": 0.12345678901234567891},
		             "refusal": {"reason": "no"}, "flags": ["steering_text"]},
		"history": [{"seq": "3", "type": "proposal.created", "payload": {"payloadSha256": "ab"}}]
	}`, string(out))
	require.Contains(t, string(out), `123456789012345678901234567890`)
	require.Contains(t, string(out), `0.12345678901234567891`)

	// A field named like JSON text that does not hold JSON stays text.
	odd, err := Structured(&agentv1.Proposal{PayloadJson: `not json`})
	require.NoError(t, err)
	require.JSONEq(t, `{"payloadJson": "not json"}`, string(odd))
}
