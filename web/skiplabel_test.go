package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every reason the server writes for why a proposal waits for a person has
// plain words in the web, so the inbox row and the detail card never show a
// raw code. A reason that ends in "_" is a prefix with an error code after
// it (refused_permission_denied), which no fixed key in skipLabel can match:
// skipText handles it, and the pages go through skipText.
func TestEverySkipReasonHasPlainWords(t *testing.T) {
	data, err := os.ReadFile("src/lib/format.ts")
	require.NoError(t, err)
	src := string(data)
	start := strings.Index(src, "export const skipLabel")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(src[start:], "\n};")
	require.Positive(t, end)
	labels := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+([a-z_]+):`).FindAllStringSubmatch(src[start:start+end], -1) {
		labels[m[1]] = true
	}
	require.NotEmpty(t, labels)

	prefix := regexp.MustCompile(`const skipRefused = "([a-z_]+)";`).FindStringSubmatch(src)
	require.Len(t, prefix, 2, "web/src/lib/format.ts has no skipRefused prefix for skipText")

	written := regexp.MustCompile(`\bSkip[A-Za-z]+\s*=\s*"([a-z_]+)"`)
	found, prefixes := 0, 0
	for _, path := range []string{"../modules/proposals/proposals.go", "../modules/mandates/mandates.go"} {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, m := range written.FindAllStringSubmatch(string(data), -1) {
			found++
			if strings.HasSuffix(m[1], "_") {
				prefixes++
				require.Equal(t, m[1], prefix[1], "%s writes the prefix %q, which skipText in web/src/lib/format.ts does not handle", path, m[1])
				continue
			}
			require.True(t, labels[m[1]], "%s writes %q, which has no entry in skipLabel in web/src/lib/format.ts", path, m[1])
		}
	}
	require.Positive(t, found)
	require.Equal(t, 1, prefixes)

	// A page that indexes skipLabel itself shows a prefixed reason raw.
	pages, err := filepath.Glob("src/pages/*.tsx")
	require.NoError(t, err)
	require.NotEmpty(t, pages)
	for _, page := range pages {
		data, err := os.ReadFile(page)
		require.NoError(t, err)
		require.False(t, strings.Contains(string(data), "skipLabel["), "%s reads skipLabel directly; use skipText so a refused_ reason gets plain words", page)
	}
}
