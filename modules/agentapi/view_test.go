package agentapi

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
)

// Every reason an agent can be told has its own explanation, keyed on the
// modules' constants; an unknown reason comes back as it is.
func TestExplainCoversEveryReason(t *testing.T) {
	all := []string{
		mandates.SkipNoMandate, mandates.SkipRiskAbove, mandates.SkipBudgetSpent, mandates.SkipConditionUnmet,
		proposals.SkipFlagged, proposals.SkipNotExecutable, proposals.SkipActionFailed, proposals.SkipExecutionFailed,
		proposals.SkipAgentRevoked, proposals.SkipActionChanged, proposals.SkipPayloadInvalid, proposals.SkipActionRemoved,
	}
	seen := map[string]string{}
	for _, reason := range all {
		text := explain(reason)
		require.NotEqual(t, reason, text, "%s has no explanation", reason)
		require.NotContains(t, seen, text, "%s explains the same as %s", reason, seen[text])
		seen[text] = reason
	}
	require.Equal(t, "some_new_reason", explain("some_new_reason"))
}

// A refusal by the database's own rules is a prefix and a code: the agent
// reads a sentence that names the code, and Skipped keeps the code itself.
func TestExplainRefusedNamesTheCode(t *testing.T) {
	reason := proposals.SkipRefused + "permission_denied"
	require.Equal(t, "the database refused to carry it out under its own rules (permission_denied), so a person must look at it", explain(reason))

	view := proposalView(proposals.Proposal{Status: proposals.StatusPending}, reason, nil)
	require.Equal(t, reason, view.Skipped)
	require.Equal(t, "waits for a person: the database refused to carry it out under its own rules (permission_denied), so a person must look at it", view.Outcome)
	require.Equal(t, pendingNext, view.Next)
}

// A pending proposal past its deadline that the sweep has not reached yet
// shows as expired, live or dry run: nobody can decide it any more.
func TestPastDeadlineShowsAsExpired(t *testing.T) {
	live := proposalView(proposals.Proposal{Status: proposals.StatusPending, PastDeadline: true}, mandates.SkipNoMandate, nil)
	require.Equal(t, proposals.StatusExpired, live.Status)
	require.Contains(t, live.Outcome, "expired: nobody decided it before ")
	require.Empty(t, live.Skipped, "an expired proposal no longer waits for a person")

	dry := proposalView(proposals.Proposal{Status: proposals.StatusPending, PastDeadline: true, DryRun: true}, mandates.SkipNoMandate, nil)
	require.Equal(t, proposals.StatusExpired, dry.Status)
	require.Contains(t, dry.Outcome, "dry run: nobody looked at it before ")
	require.Empty(t, dry.Skipped)
	require.Equal(t, dryRunReadNext, dry.Next)

	fresh := proposalView(proposals.Proposal{Status: proposals.StatusPending}, "", nil)
	require.Equal(t, proposals.StatusPending, fresh.Status)
	require.Equal(t, "waits for a person", fresh.Outcome)
}
