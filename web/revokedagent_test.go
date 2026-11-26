package web

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A revoked agent cannot propose and its tokens are refused, so its mandates
// can no longer apply. The server still lists them among those in force,
// because the mandate itself is neither revoked nor expired. The Mandates
// page must not read them as standing delegations: each such row is marked
// "agent revoked" and counted as ended, so it offers no Revoke. A mandate for
// every agent (an empty agentId) is never marked by one agent's revocation.
func TestMandatesOfARevokedAgentAreEnded(t *testing.T) {
	data, err := os.ReadFile("src/pages/MandatesPage.tsx")
	require.NoError(t, err)
	src := string(data)

	start := strings.Index(src, "function MandateRow(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(src[start:], "\nexport function MandatesPage(")
	require.Positive(t, end)
	row := src[start : start+end]
	page := src[start+end:]

	require.Contains(t, row, "agentRevoked: boolean;", "the row is told whether its agent is revoked")
	require.Contains(t, row, "const ended = !!mandate.revokedAt || expired || agentRevoked;", "a revoked agent's mandate counts as ended")
	require.Contains(t, row, "{canRevoke && !ended ? (", "an ended mandate offers no Revoke")
	require.Regexp(t, `\) : agentRevoked \? \(\s*<Badge tone="neutral">agent revoked</Badge>`, row, "the row says why it has ended")

	require.Contains(t, page, "const revokedAgents = new Set((agents.data?.agents ?? []).filter((a) => !!a.revokedAt).map((a) => a.id));")
	require.Contains(t, page, "agentRevoked={!!m.agentId && revokedAgents.has(m.agentId)}", "a mandate for every agent is not marked")
}
