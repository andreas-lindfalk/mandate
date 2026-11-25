package deliveries

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

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
	require.Equal(t, "blocked: the connector address is private or local", describe(err))

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
	require.Equal(t, "connector answered 503", describe(&statusError{status: 503}))
	require.Equal(t, "the connector could not be reached", describe(errors.New(`Post "http://user:secret@10.0.0.5:6379/x": dial tcp 10.0.0.5:6379: connect: connection refused`)))
}

func TestPlainHttpToAPublicAddressIsRefused(t *testing.T) {
	strict := &Service{client: newClient(false)}
	_, _, err := strict.post(context.Background(), "http://connector.example/hooks", []byte("secret"), uuid.New(), []byte(`{}`))
	require.ErrorIs(t, err, errPlain)
	require.Equal(t, "blocked: "+errPlain.Error(), describe(err))
	_, _, err = strict.post(context.Background(), "HTTPS://127.0.0.1:1/hooks", []byte("secret"), uuid.New(), []byte(`{}`))
	require.ErrorIs(t, err, errBlocked, "https gets past the scheme check, and on to the address check")
}
