package web

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every event type the server writes by name has plain words in eventLabel,
// so the Evidence table and a proposal's history never show a raw type.
// Types built at run time ("proposal." + status, "delivery." + outcome) are
// not found by name here and are kept in the list by hand.
func TestEveryNamedEventHasALabel(t *testing.T) {
	data, err := os.ReadFile("src/lib/format.ts")
	require.NoError(t, err)
	src := string(data)
	start := strings.Index(src, "export const eventLabel")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(src[start:], "\n};")
	require.Positive(t, end)
	labels := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z_]+\.[a-z_]+)":`).FindAllStringSubmatch(src[start:start+end], -1) {
		labels[m[1]] = true
	}

	written := regexp.MustCompile(`Type: +"([a-z_]+\.[a-z_]+)"`)
	root := os.DirFS("..")
	found := 0
	err = fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "gen", "node_modules", "web", ".git":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		for _, m := range written.FindAllStringSubmatch(string(data), -1) {
			found++
			require.True(t, labels[m[1]], "%s writes %q, which has no entry in eventLabel in web/src/lib/format.ts", path, m[1])
		}
		return nil
	})
	require.NoError(t, err)
	require.Positive(t, found)
}
