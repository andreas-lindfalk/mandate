package authlimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// a server that accepts the token "good" and refuses any other.
func behind(l *Limiter) http.Handler {
	return l.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/oidc/callback" {
			http.Error(w, "the sign-in did not match", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
}

func call(t *testing.T, h http.Handler, path, token, from string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = from + ":5555"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if len(header) == 2 {
		req.Header.Set(header[0], header[1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestTwentyFailuresAMinute(t *testing.T) {
	l := New(Options{SessionCookie: "mandate_session", FlowCookie: "mandate_oidc"})
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	h := behind(l)

	for i := range DefaultMax {
		require.Equal(t, http.StatusUnauthorized, call(t, h, "/mandate.v1.AuthService/Me", "bad", "10.0.0.1").Code, i)
	}
	limited := call(t, h, "/mandate.v1.AuthService/Me", "good", "10.0.0.1")
	require.Equal(t, http.StatusTooManyRequests, limited.Code, "a valid token from a limited address is refused too")
	require.Equal(t, "60", limited.Header().Get("Retry-After"))
	require.Contains(t, limited.Body.String(), `"code":"resource_exhausted"`, "a Connect client reads the code")
	require.Equal(t, http.StatusTooManyRequests, call(t, h, "/mcp", "good", "10.0.0.1").Code, "every door that checks credentials")
	require.Equal(t, http.StatusOK, call(t, h, "/mandate.v1.AuthService/Me", "good", "10.0.0.2").Code, "another address is not affected")
	require.Equal(t, http.StatusOK, call(t, h, "/healthz", "good", "10.0.0.1").Code, "what checks no credential is not limited")

	now = now.Add(30 * time.Second)
	require.Equal(t, "30", call(t, h, "/mcp", "good", "10.0.0.1").Header().Get("Retry-After"))
	now = now.Add(31 * time.Second)
	require.Equal(t, http.StatusOK, call(t, h, "/mcp", "good", "10.0.0.1").Code, "the window ends")
}

func TestOnlyACredentialThatFailsCounts(t *testing.T) {
	l := New(Options{SessionCookie: "mandate_session", FlowCookie: "mandate_oidc"})
	h := behind(l)
	for range 3 * DefaultMax {
		require.Equal(t, http.StatusUnauthorized, call(t, h, "/mandate.v1.AuthService/Me", "", "10.0.0.1").Code)
	}
	require.Equal(t, http.StatusOK, call(t, h, "/mandate.v1.AuthService/Me", "good", "10.0.0.1").Code, "a signed-out page asking is no failure")

	// A session cookie is a credential.
	for range DefaultMax {
		req := httptest.NewRequest(http.MethodPost, "/mandate.v1.AuthService/Me", nil)
		req.RemoteAddr = "10.0.0.3:1"
		req.AddCookie(&http.Cookie{Name: "mandate_session", Value: "stale"})
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	require.Equal(t, http.StatusTooManyRequests, call(t, h, "/mandate.v1.AuthService/Me", "good", "10.0.0.3").Code)

	// A failed sign-in callback counts only when the browser started the
	// sign-in: a page elsewhere can make a browser fetch the callback, but
	// not with the flow's cookie.
	for range 3 * DefaultMax {
		require.Equal(t, http.StatusBadRequest, call(t, h, "/auth/oidc/callback", "", "10.0.0.4").Code)
	}
	require.Equal(t, http.StatusOK, call(t, h, "/mcp", "good", "10.0.0.4").Code, "a forced callback locks nobody out")
	for range DefaultMax {
		req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
		req.RemoteAddr = "10.0.0.5:1"
		req.AddCookie(&http.Cookie{Name: "mandate_oidc", Value: "state.verifier"})
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	require.Equal(t, http.StatusTooManyRequests, call(t, h, "/auth/oidc/callback", "", "10.0.0.5").Code)
}

// gRPC and gRPC-Web answer a refusal with HTTP 200, which would go
// uncounted, so they are refused where credentials are checked.
func TestGrpcIsRefused(t *testing.T) {
	h := behind(New(Options{}))
	for _, contentType := range []string{"application/grpc", "application/grpc-web+proto", "application/grpc-web-text"} {
		req := httptest.NewRequest(http.MethodPost, "/mandate.v1.AuthService/Me", nil)
		req.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnsupportedMediaType, w.Code, contentType)
	}
	req := httptest.NewRequest(http.MethodPost, "/mandate.v1.AuthService/Me", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer good")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "Connect is served")
}

// Many addresses cannot grow the map without bound: past its capacity a
// new address goes uncounted until a sweep, at most once a window, frees
// room.
func TestTheMapIsBounded(t *testing.T) {
	l := New(Options{})
	l.capacity = 3
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	for _, a := range []string{"a", "b", "c", "d", "e"} {
		l.fail(a)
	}
	require.Len(t, l.entries, 3)
	now = now.Add(DefaultWindow)
	l.fail("f")
	require.Len(t, l.entries, 1, "the old entries were swept, once the window passed")
	require.Contains(t, l.entries, "f")
}

func TestTheAddressHeaderOnlyWhenConfigured(t *testing.T) {
	direct := New(Options{})
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.7")
	require.Equal(t, "192.0.2.1", direct.ClientAddress(req), "a header nobody configured is not believed")
	proxied := New(Options{Header: "X-Forwarded-For"})
	require.Equal(t, "198.51.100.7", proxied.ClientAddress(req), "the last entry, the one the trusted proxy wrote")
	req.Header.Add("X-Forwarded-For", "198.51.100.8")
	require.Equal(t, "198.51.100.8", proxied.ClientAddress(req), "a line the proxy added is the last line, not the client's first")
	req.Header.Del("X-Forwarded-For")
	require.Equal(t, "192.0.2.1", proxied.ClientAddress(req), "without the header, the connection's")
	req.RemoteAddr = "[2001:db8:1:2:3:4:5:6]:443"
	require.Equal(t, "2001:db8:1:2::/64", proxied.ClientAddress(req), "an IPv6 client counts by its /64")

	// Behind a proxy, each client is counted apart.
	h := behind(proxied)
	for range DefaultMax {
		call(t, h, "/mcp", "bad", "192.0.2.1", "X-Forwarded-For", "203.0.113.1")
	}
	require.Equal(t, http.StatusTooManyRequests, call(t, h, "/mcp", "good", "192.0.2.1", "X-Forwarded-For", "203.0.113.1").Code)
	require.Equal(t, http.StatusOK, call(t, h, "/mcp", "good", "192.0.2.1", "X-Forwarded-For", "203.0.113.2").Code)
}
