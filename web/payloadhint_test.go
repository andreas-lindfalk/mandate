package web

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// While the approver edits the payload, the line under it says why Approve
// is off ("This is not valid JSON.") or how the edit is checked. A screen
// reader hears it from the textarea, and from Approve while it is disabled by
// a payload error, as Reject does with its "reject-why".
func TestPayloadHintIsTiedToTheEditorAndApprove(t *testing.T) {
	src := readSource(t, "src/pages/InboxPage.tsx")

	require.Contains(t, src, `aria-label="Payload, edited before approval" aria-invalid={!!payloadError} aria-describedby="payload-hint" />`,
		"the textarea is described by the hint under it")
	require.Contains(t, src, `<p id="payload-hint" className={cn("mt-1 text-[12px]", payloadError ? "text-danger-ink" : "text-muted")}> {payloadError ||`,
		"the hint carries the id and the payload error")
	require.Contains(t, src, `disabled={(needsConfirm && !confirm) || !!payloadError} aria-describedby={payloadError ? "payload-hint" : undefined} >`,
		"Approve, disabled by a payload error, points at the reason")
}
