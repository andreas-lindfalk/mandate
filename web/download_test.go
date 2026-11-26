package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// An export hands the file to the browser through a blob URL. WebKit can drop
// the download when that URL is revoked before the download has started, and
// nothing tells the auditor: the toast only covers a failed API call. So the
// revoke is deferred, never run right after click().
func TestExportsRevokeTheBlobURLAfterTheDownloadStarts(t *testing.T) {
	space := regexp.MustCompile(`\s+`)
	deferred := regexp.MustCompile(`window\.setTimeout\(\(\) => URL\.revokeObjectURL\(\w+\), [\d_]+\)`)
	var pages []string
	err := filepath.WalkDir("src", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx")) {
			pages = append(pages, path)
		}
		return err
	})
	require.NoError(t, err)
	require.NotEmpty(t, pages)

	downloads := 0
	for _, page := range pages {
		data, err := os.ReadFile(page)
		require.NoError(t, err)
		src := space.ReplaceAllString(string(data), " ")
		if !strings.Contains(src, "URL.createObjectURL(") {
			continue
		}
		downloads++
		revokes := strings.Count(src, "URL.revokeObjectURL(")
		require.Positive(t, revokes, "%s creates a blob URL and never revokes it", page)
		require.Len(t, deferred.FindAllString(src, -1), revokes, "%s revokes a blob URL without deferring it, which can cancel the download in WebKit", page)
		require.Contains(t, src, "document.body.append(a); a.click(); a.remove();", "%s clicks a download link that is not in the document", page)
	}
	require.Equal(t, 1, downloads, "the evidence page's export is the one download")
}
