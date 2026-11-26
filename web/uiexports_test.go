package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every component the ui folder exports is used by some other file in the
// web app. An export nothing imports is a hook left unfinished, and it reads
// as if the app had a behaviour it does not have.
func TestEveryUIExportIsUsed(t *testing.T) {
	var others []string
	err := filepath.WalkDir("src", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path == filepath.Join("src", "gen") {
			return filepath.SkipDir
		}
		if !d.IsDir() && (strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx")) {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			others = append(others, path+"\x00"+string(data))
		}
		return nil
	})
	require.NoError(t, err)

	files, err := filepath.Glob("src/components/ui/*.tsx")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	export := regexp.MustCompile(`(?m)^export (?:const|function) ([A-Za-z0-9_]+)`)
	checked := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, m := range export.FindAllStringSubmatch(string(data), -1) {
			checked++
			word := regexp.MustCompile(`\b` + m[1] + `\b`)
			used := false
			for _, o := range others {
				path, src, _ := strings.Cut(o, "\x00")
				if path != file && word.MatchString(src) {
					used = true
					break
				}
			}
			require.True(t, used, "%s exports %s, which no other file in web/src uses; remove it", file, m[1])
		}
	}
	require.Positive(t, checked)
}
