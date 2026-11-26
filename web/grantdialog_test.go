package web

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The grant dialog's preview says what the mandate lets through among the
// actions defined today. While the actions or the agents are still loading,
// or failed to load, an empty list would read as "nothing that is defined
// today" or as only "Every agent", and the owner would tick "I am
// accountable" for a false statement. So each list shows a skeleton while it
// loads and an ErrorState with a retry when it failed, "No actions are
// defined yet" only follows a load, and the preview with its tick, and the
// grant itself, wait for both lists.
func TestGrantDialogWaitsForItsLists(t *testing.T) {
	data, err := os.ReadFile("src/pages/MandatesPage.tsx")
	require.NoError(t, err)
	src := string(data)
	start := strings.Index(src, "function GrantDialog(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(src[start:], "\nfunction ")
	require.Positive(t, end)
	dialog := src[start : start+end]

	require.Contains(t, dialog, "const loaded = !!actions.data && !!agents.data;")
	require.Regexp(t, `const ready = loaded && `, dialog, "the grant waits for both lists")

	require.Regexp(t, `\{agents\.data \? \(\s*<Select id="agent"`, dialog, "the agent choice is offered only from a loaded list")
	require.Contains(t, dialog, `<ErrorState className="py-4" title="Could not load the agents" error={agents.error} onRetry={() => agents.refetch()} />`)

	require.Regexp(t, `\{!actions\.data \? \(\s*actions\.isError \? \(\s*<ErrorState className="py-4" title="Could not load the actions" error=\{actions\.error\} onRetry=\{\(\) => actions\.refetch\(\)\} />\s*\) : \(\s*<Skeleton`, dialog,
		"the action list shows a failure or a skeleton until it has loaded")
	require.Regexp(t, `\) : defined\.length === 0 \? \(\s*<p[^>]*>No actions are defined yet`, dialog, `"none defined" follows only a loaded list`)

	cardAt := strings.Index(dialog, `<Card className="bg-warning-soft`)
	require.Positive(t, cardAt)
	require.Contains(t, dialog, "{agentId && patterns.length > 0 && loaded ? (\n            <Card", "the preview and its tick wait for both lists")
	require.Equal(t, 1, strings.Count(dialog, "I am accountable for this delegation"), "the tick appears only in the guarded preview")
	require.Less(t, strings.Index(dialog, "&& loaded ? ("), cardAt)
	require.Greater(t, strings.Index(dialog, "I am accountable for this delegation"), cardAt)
}
