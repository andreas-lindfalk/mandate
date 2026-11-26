package web

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ListEvents returns at most the limit it is asked for and no total, and an
// agent.tool_called event is written for every tool call, so a busy tenant
// passes the Evidence table's limit quickly. A full table must say that it
// shows only the newest events and point to the chain export, or an auditor
// who filters on a subject has no reason to think there are more.
func TestTheEvidenceTableSaysWhenItIsCutShort(t *testing.T) {
	data, err := os.ReadFile("src/pages/EvidencePage.tsx")
	require.NoError(t, err)
	src := string(data)

	limit := regexp.MustCompile(`const eventLimit = (\d+);`).FindStringSubmatch(src)
	require.Len(t, limit, 2, "EvidencePage.tsx has no eventLimit constant")
	require.Contains(t, src, "api.evidence.listEvents({ subjectId: applied, limit: eventLimit })",
		"the events query asks for eventLimit, the number the note is about")
	require.NotRegexp(t, `listEvents\(\{[^}]*limit:\s*\d`, src, "the events query has a literal limit beside eventLimit")

	table := strings.Index(src, "</table>")
	require.Positive(t, table)
	note := regexp.MustCompile(`\{list\.length >= eventLimit \? \(\s*<p className="[^"]*text-muted[^"]*">Showing the newest \{eventLimit\} events\. Export the chain for all of them\.</p>`)
	at := note.FindStringIndex(src)
	require.NotNil(t, at, "a full Evidence table has no note that it shows only the newest events")
	require.Greater(t, at[0], table, "the note sits under the table")
}
