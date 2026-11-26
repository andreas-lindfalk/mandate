package web

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// In TanStack Query v5 isError is also true after a failed background
// refetch, while the data from the earlier load is kept. Refetches happen on
// every window focus and after each mutation's invalidate, so a page that
// tests isError before data would replace a list that is on screen with an
// error, and unmount what the owner was doing in it (an open limits edit, an
// armed revoke). A full ErrorState is therefore only for a query without
// data, and a failed refetch shows a StaleNote beside the data it kept.
func TestAFailedRefetchKeepsTheDataOnScreen(t *testing.T) {
	errorFirst := regexp.MustCompile(`(\w+)\.isError\s*\?\s*\(?\s*<ErrorState`)
	err := fs.WalkDir(os.DirFS("src"), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tsx") {
			return err
		}
		data, err := os.ReadFile("src/" + path)
		require.NoError(t, err)
		src := string(data)
		for _, m := range errorFirst.FindAllStringSubmatchIndex(src, -1) {
			name := src[m[2]:m[3]]
			before := src[max(0, m[0]-200):m[0]]
			// The error branch must sit under a check of the same query's
			// data, as in `!q.data ? (q.isError ? (<ErrorState` or
			// `q.data ? (...) : q.isError ? (<ErrorState`.
			require.True(t, strings.Contains(before, name+".data ? ("),
				"%s: %s.isError shows an ErrorState without first checking %s.data, so a failed refetch hides loaded data", path, name, name)
		}
		return nil
	})
	require.NoError(t, err)

	for page, queries := range map[string][]string{
		"src/pages/AgentsPage.tsx":   {"tokens", "agents", "actions"},
		"src/pages/MandatesPage.tsx": {"mandates"},
		"src/pages/PeoplePage.tsx":   {"notifier", "people"},
		"src/pages/InboxPage.tsx":    {"list"},
		"src/pages/EvidencePage.tsx": {"events"},
	} {
		data, err := os.ReadFile(page)
		require.NoError(t, err)
		src := string(data)
		for _, q := range queries {
			guarded := regexp.MustCompile(`\) : !` + q + `\.data \? \(\s*<ErrorState [^>]*error=\{` + q + `\.error\}`)
			require.True(t, guarded.MatchString(src),
				"%s: %s shows its error only when it has no data", page, q)
			require.True(t, strings.Contains(src, "{"+q+".isError && "+q+".data ? <StaleNote error={"+q+".error}"),
				"%s: a failed refetch of %s says the data it kept may be out of date", page, q)
		}
	}
}

// VerifyChain is a full scan of the chain and the most likely call to time
// out. A failed re-check must keep the last result on screen, above all a
// broken one, and not turn it into a neutral "could not check".
func TestAFailedChainRecheckKeepsTheLastResult(t *testing.T) {
	shell, err := os.ReadFile("src/components/layout/AppShell.tsx")
	require.NoError(t, err)
	src := string(shell)
	start := strings.Index(src, "function ChainStatus()")
	require.GreaterOrEqual(t, start, 0, "AppShell has a ChainStatus")
	end := strings.Index(src[start:], "\nfunction ")
	require.Greater(t, end, 0)
	status := src[start : start+end]
	noData := strings.Index(status, "if (!chain.data) {")
	require.GreaterOrEqual(t, noData, 0, "the sidebar says it could not check only when it has no result")
	isError := strings.Index(status, "chain.isError")
	require.Greater(t, isError, noData, "the sidebar tests chain.isError only under a check of chain.data")
	require.Contains(t, status[noData:], "Could not check the audit trail right now")
	broken := strings.Index(status, `"The audit trail is broken"`)
	require.Greater(t, broken, 0)
	require.Contains(t, status[broken:], "{chain.isError ? <span", "with a result at hand, a failed re-check is a note beside it")

	evidence, err := os.ReadFile("src/pages/EvidencePage.tsx")
	require.NoError(t, err)
	page := string(evidence)
	require.Regexp(t, `chain\.isPending \? \(\s*"Checking the audit trail…"\s*\) : !chain\.data \? \(\s*"Could not check the audit trail right now\."`, page,
		"Evidence says it could not check only when it has no result")
	require.NotContains(t, page, ": chain.isError ? (", "Evidence does not let a failed re-check hide the last result")
	require.Contains(t, page, "{chain.isError && chain.data ? <StaleNote error={chain.error}",
		"a failed re-check on Evidence says the result it kept may be out of date")
}
