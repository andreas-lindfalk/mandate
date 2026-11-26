package notifications

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
)

// An owner names agents and actions; even so, a name cannot ping a channel
// or break out of the formatting in either tool.
func TestRenderEscapes(t *testing.T) {
	s := &Service{publicUrl: "https://pgmandate.example"}
	m := Message{ProposalId: uuid.New(), Action: "refunds.issue", Risk: "medium", Agent: "<!channel> & @everyone *bot*", Reason: "condition_not_met"}

	slack := s.render(KindSlack, m)["text"].(string)
	require.Contains(t, slack, "&lt;!channel&gt; &amp; @everyone *bot*")
	require.NotContains(t, slack, "<!channel>")
	require.Contains(t, slack, "the payload does not meet the mandate's condition")
	require.Contains(t, slack, "<https://pgmandate.example/inbox/"+m.ProposalId.String()+"|Open it in pgmandate>")

	discord := s.render(KindDiscord, m)
	content := discord["content"].(string)
	require.Contains(t, content, `\<!channel> & @`+"\u200b"+`everyone \*bot\*`)
	require.NotContains(t, content, "@everyone")
	require.Equal(t, map[string]any{"parse": []string{}}, discord["allowed_mentions"])

	refused := s.render(KindSlack, Message{ProposalId: m.ProposalId, Action: "a.b", Risk: "low", Agent: "x", Reason: proposals.SkipRefused + "permission_denied"})["text"].(string)
	require.Contains(t, refused, " Why: the database refused to carry it out under its own rules (permission_denied).")
	require.NotContains(t, refused, "it was left for a person")
	unknown := s.render(KindSlack, Message{ProposalId: m.ProposalId, Action: "a.b", Risk: "low", Agent: "x", Reason: "some_new_reason"})["text"].(string)
	require.Contains(t, unknown, "it was left for a person")
	bare := (&Service{}).render(KindSlack, Message{ProposalId: m.ProposalId, Action: "a.b", Risk: "low", Agent: "x"})["text"].(string)
	require.NotContains(t, bare, "Why")
	require.NotContains(t, bare, "Open it")
}

// Every reason a proposal can wait for has words for a person, keyed on the
// modules' own constants so the two cannot drift apart: the ones the chain
// records, and action_removed, which is worked out when the action is gone.
func TestEveryRecordedReasonHasWords(t *testing.T) {
	recorded := []string{
		mandates.SkipNoMandate, mandates.SkipRiskAbove, mandates.SkipBudgetSpent, mandates.SkipConditionUnmet,
		proposals.SkipFlagged, proposals.SkipNotExecutable, proposals.SkipActionFailed, proposals.SkipExecutionFailed,
		proposals.SkipAgentRevoked, proposals.SkipActionChanged, proposals.SkipPayloadInvalid, proposals.SkipActionRemoved,
	}
	require.Len(t, reasons, len(recorded))
	s := &Service{}
	for _, reason := range recorded {
		require.NotEmpty(t, reasons[reason], reason)
		text := s.render(KindSlack, Message{ProposalId: uuid.New(), Action: "a.b", Risk: "low", Agent: "x", Reason: reason})["text"].(string)
		require.Contains(t, text, reasons[reason], reason)
		require.NotContains(t, text, "it was left for a person", reason)
	}
}
