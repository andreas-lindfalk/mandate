package web

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Revoking an agent sets revoked_at on the agent row and leaves its tokens'
// own revoked_at empty; resolve_agent_token stops them through the agent. A
// token's state worked out from the token alone would show the tokens of a
// revoked agent as Active, with an "until" date and a revoke button. The
// state must take the agent into account, and the card must pass it.
func TestATokenOfARevokedAgentIsNotShownAsActive(t *testing.T) {
	data, err := os.ReadFile("src/pages/AgentsPage.tsx")
	require.NoError(t, err)
	src := string(data)

	fn := regexp.MustCompile(`(?s)function tokenState\(token: Token, agent: Agent\)[^{]*\{(.*?)\n\}`).FindStringSubmatch(src)
	require.NotNil(t, fn, "tokenState takes the agent as well as the token")
	body := fn[1]
	agentCheck := strings.Index(body, `if (agent.revokedAt) return { label: "Ended with the agent", tone: "neutral" }`)
	require.GreaterOrEqual(t, agentCheck, 0, "a token of a revoked agent says it ended with the agent")
	active := strings.Index(body, `label: "Active"`)
	require.Greater(t, active, agentCheck, "the agent is checked before a token can be called Active")
	expired := strings.Index(body, `label: "Expired"`)
	require.Greater(t, expired, agentCheck, "a token of a revoked agent is not called Expired either")

	require.True(t, strings.Contains(src, "tokenState(token, agent)"), "the card passes its agent to tokenState")
	require.False(t, strings.Contains(src, "tokenState(token)"), "no call works out a token's state from the token alone")
	// The "until" date and the revoke button hang on the Active label, so a
	// token that ended with its agent shows neither.
	require.True(t, strings.Contains(src, "{state.label === \"Active\" ? `until ${formatDateTime(token.expiresAt)}` : \"\"}"), "the until date shows only for an Active token")
	require.True(t, strings.Contains(src, `{isOwner && state.label === "Active" ? (`), "the revoke button shows only for an Active token")
}
