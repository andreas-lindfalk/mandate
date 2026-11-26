package web

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The product is called pgmandate everywhere a person reads it. "mandate"
// alone is the thing an agent is granted, so text that names the product
// ("Could not reach mandate", "went wrong in mandate") is a leftover from
// the rename and reads as if a mandate were a server.
func TestTheWebNamesTheProductPgmandate(t *testing.T) {
	bare := regexp.MustCompile(`\b(reach|in|to|from|of|open) mandate\b`)
	checked := 0
	err := fs.WalkDir(os.DirFS("src"), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "gen" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx") {
			return nil
		}
		data, err := os.ReadFile("src/" + path)
		if err != nil {
			return err
		}
		checked++
		for i, line := range strings.Split(string(data), "\n") {
			require.False(t, bare.MatchString(line), "web/src/%s:%d names the product mandate: %s", path, i+1, strings.TrimSpace(line))
		}
		return nil
	})
	require.NoError(t, err)
	require.Positive(t, checked)

	// errorText is the body of every toast and ErrorState for a network or
	// unknown error, next to titles that say pgmandate.
	data, err := os.ReadFile("src/lib/errors.ts")
	require.NoError(t, err)
	src := string(data)
	require.Equal(t, 2, strings.Count(src, `"Could not reach pgmandate. Check the connection."`))
	require.Contains(t, src, `"Something went wrong in pgmandate."`)
}
