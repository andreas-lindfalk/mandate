package deliveries

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A connector may not live on this machine or a private network unless the
// operator said so, and a redirect is never followed.
func TestClientRefusesPrivateAddressesAndRedirects(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := newClient(false).Post(target.URL, "application/json", nil) //nolint:noctx,bodyclose // a test; the call is refused, so there is no body
	require.ErrorIs(t, err, errBlocked, "loopback is refused by default")
	require.Zero(t, hits)
	require.Equal(t, "blocked: the receiver's address is private or local", describe(err, 0))

	resp, err := newClient(true).Post(target.URL, "application/json", nil) //nolint:noctx // a test
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, hits)

	resp, err = newClient(true).Post(redirector.URL, "application/json", nil) //nolint:noctx // a test
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode, "the redirect is the answer, not followed")
	require.Equal(t, 1, hits)
}

func TestBlockedAddresses(t *testing.T) {
	for address, blocked := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.9": true, "192.168.1.1": true, "169.254.169.254": true,
		"100.64.0.1": true, "0.1.2.3": true, "198.18.0.1": true, "240.0.0.1": true, "192.0.0.8": true,
		"::1": true, "fc00::1": true, "fd12:3456::1": true, "fe80::1": true, "::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true,
		"64:ff9b::a00:1": true, "2002:a00:1::1": true, "::": true, "fec0::1": true, "64:ff9b:1::a00:1": true, "2001:0:4136:e378::1": true,
		"93.184.216.34": false, "8.8.8.8": false, "2606:2800:220:1::1": false, "::ffff:8.8.8.8": false,
	} {
		require.Equal(t, blocked, blockedAddr(netip.MustParseAddr(address)), address)
	}
}

func TestDescribeNeverLeaksTheAddress(t *testing.T) {
	require.Equal(t, "the receiver answered 503 without an outcome", describe(nil, 503))
	require.Equal(t, "the receiver could not be reached", describe(errors.New(`Post "http://user:secret@10.0.0.5:6379/x": dial tcp 10.0.0.5:6379: connect: connection refused`), 0))
}

func TestPlainHttpToAPublicAddressIsRefusedUnsent(t *testing.T) {
	strict := &Service{client: newClient(false)}
	r := strict.post(context.Background(), "http://connector.example/hooks", uuid.New(), []byte(`{}`), 1, "v2 kid=x sig=y", time.Second)
	require.ErrorIs(t, r.err, errPlain)
	require.False(t, r.sent, "refused before anything was written")
	require.Equal(t, "blocked: "+errPlain.Error(), describe(r.err, 0))
	r = strict.post(context.Background(), "HTTPS://127.0.0.1:1/hooks", uuid.New(), []byte(`{}`), 1, "v2 kid=x sig=y", time.Second)
	require.ErrorIs(t, r.err, errBlocked, "https gets past the scheme check, and on to the address check")
	require.False(t, r.sent)
}

// Whether an attempt may have reached the receiver is taken from whether
// its headers were written: a refused connection did not; a server that
// takes the request and never answers may have acted.
func TestAnAttemptIsSentOnceItsHeadersAreWritten(t *testing.T) {
	s := &Service{client: newClient(true), allowPrivate: true}
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()
	r := s.post(context.Background(), url, uuid.New(), []byte(`{}`), 1, "v2 kid=x sig=y", time.Second)
	require.Error(t, r.err)
	require.False(t, r.sent, "nothing listened: proven unsent")

	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	defer slow.Close()
	defer close(hang)
	r = s.post(context.Background(), slow.URL, uuid.New(), []byte(`{}`), 1, "v2 kid=x sig=y", time.Second)
	require.Error(t, r.err)
	require.True(t, r.sent, "it arrived and timed out: possibly done")
}

// Only an explicit answer is final.
func TestOnlyAnExplicitAnswerIsFinal(t *testing.T) {
	for name, c := range map[string]struct {
		status  int
		body    string
		outcome string
	}{
		"done":                {200, `{"outcome":"done","result":{"id":"R-1"}}`, StatusDone},
		"done without result": {200, `{"outcome":"done"}`, StatusDone},
		"refused with 422":    {422, `{"outcome":"refused","refusal":{"reason":"no such order"}}`, StatusRefused},
		"refused with 200":    {200, `{"outcome":"refused","refusal":{"reason":"no"}}`, StatusRefused},
		"unknown":             {200, `{"outcome":"unknown","reason":"the bank timed out"}`, StatusUnknown},
		"a proxy's 404":       {404, `<html>not found</html>`, ""},
		"a bare 404 json":     {404, `{"error":"no route"}`, ""},
		"a 2xx without one":   {200, `{"id":"R-1"}`, ""},
		"done with a 500":     {500, `{"outcome":"done"}`, ""},
		"unknown with a 409":  {409, `{"outcome":"unknown"}`, ""},
		"a redirect":          {307, ``, ""},
		"something else":      {200, `{"outcome":"maybe"}`, ""},
	} {
		outcome, _ := explicitAnswer(c.status, []byte(c.body))
		require.Equal(t, c.outcome, outcome, name)
	}
	_, kept := explicitAnswer(200, []byte(`{"outcome":"done","result":{"id":"R-1"}}`))
	require.JSONEq(t, `{"id":"R-1"}`, string(kept))
	_, kept = explicitAnswer(422, []byte(`{"outcome":"refused","refusal":{"reason":"x"}}`))
	require.JSONEq(t, `{"reason":"x"}`, string(kept))
}
