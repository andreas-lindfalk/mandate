package proposals

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
)

// A proposal whose action is gone waits for that reason, whatever the
// action it was made under would have said; otherwise the action decides.
func TestWaitReason(t *testing.T) {
	executable := Action{Risk: mandates.RiskLow, MayExecute: true, Webhook: &Webhook{Url: "https://connector.example/hooks"}}
	low := Proposal{Risk: mandates.RiskLow}
	require.Equal(t, SkipActionRemoved, WaitReason(Action{}, false, low))
	require.Equal(t, SkipActionRemoved, WaitReason(executable, false, Proposal{Risk: mandates.RiskHigh}))
	require.Equal(t, SkipNotExecutable, WaitReason(Action{Risk: mandates.RiskLow}, true, low))
	require.Equal(t, mandates.SkipRiskAbove, WaitReason(executable, true, Proposal{Risk: mandates.RiskHigh}))
	require.Empty(t, WaitReason(executable, true, low), "a candidate waits for no reason of its own")
}
