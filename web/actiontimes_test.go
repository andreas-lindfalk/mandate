package web

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The server reads a wait or a delivery time of 0 as "not given" and puts its
// default in its place (7 days, 60 minutes). An emptied field used to be sent
// as 0, so editing an action that waits an hour and clearing the field saved
// it as waiting seven days, without a word. So both fields are required, the
// times are worked out in seconds before the save, Save stays disabled with a
// message while either is not above 0, and nothing falls back to 0.
func TestDefineActionNeverSendsAnEmptyTimeAsZero(t *testing.T) {
	data, err := os.ReadFile("src/pages/AgentsPage.tsx")
	require.NoError(t, err)
	src := string(data)
	start := strings.Index(src, "function DefineActionDialog(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(src[start:], "\nfunction ")
	require.Positive(t, end)
	dialog := src[start : start+end]

	require.NotContains(t, dialog, "|| 0", "an empty or invalid time must not become 0")
	require.Contains(t, dialog, `const seconds = (value: string, unit: number) => (value.trim() === "" ? NaN : Math.round(Number(value) * unit));`)
	require.Contains(t, dialog, "const waitSeconds = seconds(waitHours, 3600);")
	require.Contains(t, dialog, "const deliverSeconds = seconds(deliverMinutes, 60);")
	require.Contains(t, dialog, "const timesProblem = !(Number.isSafeInteger(waitSeconds) && waitSeconds > 0) || !(Number.isSafeInteger(deliverSeconds) && deliverSeconds > 0) ?",
		"a time that rounds to 0 seconds, is empty or not a number, or is too large for BigInt to take (such as 1e309, Infinity), is a problem")
	require.Contains(t, dialog, "waitSeconds: BigInt(waitSeconds),")
	require.Contains(t, dialog, "deliverSeconds: BigInt(deliverSeconds),")

	require.Regexp(t, `<Input id="action-wait" type="number" required `, dialog)
	require.Regexp(t, `<Input id="action-deliver" type="number" required `, dialog)
	require.Contains(t, dialog, "if (schemaError || timesProblem) return;", "a submit with a problem time sends nothing")
	require.Contains(t, dialog, "disabled={!key.trim() || !!timesProblem || (schemaTouched && !!schemaError)}", "Save waits for both times")
	require.Contains(t, dialog, `aria-describedby={timesProblem ? "action-times-problem" : undefined}`)
	require.Regexp(t, `\{timesProblem \? \(\s*<p id="action-times-problem"[^>]*>\s*\{timesProblem\}`, dialog, "the reason is said beside the fields")
}
