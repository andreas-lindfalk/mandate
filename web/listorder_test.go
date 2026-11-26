package web

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The note under a cut-off inbox list says which end it shows, so it must
// follow the order the list was fetched in. The dry-run tab waits like the
// pending tab and is fetched oldest first under another tab key: a note that
// goes by the tab key would call it newest first.
func TestInboxCutOffNoteFollowsFetchOrder(t *testing.T) {
	data, err := os.ReadFile("src/pages/InboxPage.tsx")
	require.NoError(t, err)
	src := string(data)

	fetch := regexp.MustCompile(`listProposals\(\{[^}]*\boldestFirst(?:\s*:\s*([^,}]+))?\s*[,}]`).FindStringSubmatch(src)
	require.Len(t, fetch, 2, "InboxPage.tsx: the list query sets no oldestFirst")
	order := strings.TrimSpace(fetch[1])
	if order == "" {
		order = "oldestFirst" // shorthand property
	}

	notes := regexp.MustCompile(`\{([^{}?]+)\?\s*"oldest"\s*:\s*"newest"\s*\}`).FindAllStringSubmatch(src, -1)
	require.Len(t, notes, 1, "InboxPage.tsx: expected one oldest/newest note")
	require.Equal(t, order, strings.TrimSpace(notes[0][1]),
		"InboxPage.tsx: the cut-off note decides oldest/newest differently from the query's oldestFirst")

	// The order goes by status, which the dry-run tab shares with pending.
	require.Contains(t, src, `{ key: "dry_run", label: "Dry runs", status: "pending", dryRun: true }`)
	if order == "oldestFirst" {
		def := regexp.MustCompile(`const oldestFirst = ([^;]+);`).FindStringSubmatch(src)
		require.Len(t, def, 2, "InboxPage.tsx: oldestFirst is not defined")
		order = strings.TrimSpace(def[1])
	}
	require.Equal(t, `current.status === "pending"`, order)
}
