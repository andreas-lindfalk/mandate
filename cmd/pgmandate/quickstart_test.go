package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTheQuickstartJobRunsForEveryPackageInTheBinary keeps the path filter of
// the quickstart workflow in step with the binary: a pull request that changes
// any of this module's packages the image is built from builds the image and
// walks deploy/quickstart/smoke.sh, as docs/running.md promises.
func TestTheQuickstartJobRunsForEveryPackageInTheBinary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	require.NoError(t, err)
	const module = "github.com/andreas-lindfalk/pgmandate"
	needed := map[string]bool{}
	for _, pkg := range strings.Fields(string(out)) {
		switch {
		case pkg == module:
			needed["*.go"] = true
		case strings.HasPrefix(pkg, module+"/"):
			top, _, _ := strings.Cut(strings.TrimPrefix(pkg, module+"/"), "/")
			needed[top+"/**"] = true
		}
	}
	require.True(t, needed["cmd/**"], "go list names the binary's own package")
	// gen/ is written by make gen from proto/, which the filter names.
	delete(needed, "gen/**")

	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "quickstart.yml"))
	require.NoError(t, err)
	_, rest, found := strings.Cut(string(workflow), "\n  pull_request:\n    paths:\n")
	require.True(t, found, "the workflow filters pull requests by path")
	filter, _, _ := strings.Cut(rest, "\njobs:")
	var paths []string
	for _, m := range regexp.MustCompile(`(?m)^\s+- "([^"]+)"$`).FindAllStringSubmatch(filter, -1) {
		paths = append(paths, m[1])
	}
	require.NotEmpty(t, paths)

	var missing []string
	for path := range needed {
		if !slices.Contains(paths, path) {
			missing = append(missing, path)
		}
	}
	slices.Sort(missing)
	require.Empty(t, missing, "the quickstart job does not run when these change")
}
