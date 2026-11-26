package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHelpNamesEveryVariableTheBinaryReads keeps the Environment block of
// usage in step with the code: every MANDATE_* name the binary's source
// quotes on its own, as an environment lookup does, is listed there.
func TestHelpNamesEveryVariableTheBinaryReads(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	require.NoError(t, err)
	read := map[string]bool{}
	quoted := regexp.MustCompile(`"(MANDATE_[A-Z0-9_]+)"`)
	for _, file := range sources {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		text, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, m := range quoted.FindAllStringSubmatch(string(text), -1) {
			read[m[1]] = true
		}
	}
	require.NotEmpty(t, read)

	_, env, found := strings.Cut(usage, "\nEnvironment:\n")
	require.True(t, found, "usage has an Environment block")
	listed := regexp.MustCompile(`\bMANDATE_[A-Z0-9_]+\b`).FindAllString(env, -1)
	var missing []string
	for name := range read {
		if !slices.Contains(listed, name) {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	require.Empty(t, missing, "pgmandate help does not list these variables")
}
