package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestHandlerServesFilesAndTheIndexForRoutes(t *testing.T) {
	root := fstest.MapFS{
		"index.html":          {Data: []byte("<html>app</html>")},
		"assets/app-abc12.js": {Data: []byte("console.log(1)")},
		"favicon.svg":         {Data: []byte("<svg/>")},
	}
	handler := newHandler(root)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	index := get("/inbox/123")
	require.Equal(t, http.StatusOK, index.Code)
	require.Equal(t, "<html>app</html>", index.Body.String(), "client side routes get the app")
	require.Equal(t, "no-cache", index.Header().Get("Cache-Control"))
	require.Equal(t, "DENY", index.Header().Get("X-Frame-Options"))

	asset := get("/assets/app-abc12.js")
	require.Equal(t, http.StatusOK, asset.Code)
	require.Contains(t, asset.Header().Get("Cache-Control"), "immutable", "hashed assets are cached forever")

	icon := get("/favicon.svg")
	require.Equal(t, http.StatusOK, icon.Code)
	require.Empty(t, icon.Header().Get("Cache-Control"))

	dir := get("/assets/")
	require.Equal(t, "<html>app</html>", dir.Body.String(), "a directory is not listed")
}

func TestHandlerSaysHowToBuildWhenThereIsNoApp(t *testing.T) {
	rec := httptest.NewRecorder()
	newHandler(fstest.MapFS{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "make web")
}
