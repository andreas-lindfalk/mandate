package notifications

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
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

	unknown := s.render(KindSlack, Message{ProposalId: m.ProposalId, Action: "a.b", Risk: "low", Agent: "x", Reason: "refused_conflict"})["text"].(string)
	require.Contains(t, unknown, "it was left for a person")
	bare := (&Service{}).render(KindSlack, Message{ProposalId: m.ProposalId, Action: "a.b", Risk: "low", Agent: "x"})["text"].(string)
	require.NotContains(t, bare, "Why")
	require.NotContains(t, bare, "Open it")
}
