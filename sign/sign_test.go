package sign

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func seed(fill byte) []byte {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = fill + byte(i)
	}
	return s
}

// TestVector is the example in docs/receivers.md, so a receiver written in any
// language can check its own verification against a known answer.
func TestVector(t *testing.T) {
	const (
		seedB64   = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
		publicB64 = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
		kid       = "56475aa75463474c"
		timestamp = int64(1767225600)
		body      = `{"version":2,"kind":"delivery","deliveryId":"00000000-0000-0000-0000-000000000001"}`
		want      = "v2 kid=56475aa75463474c sig=RQk5h/yRhlTstIABLiW11fXXFg/5FmlDJtWnmdagCNPieC5C5ZCfUVsT9eXzRY9JlMdqEz1R0g3MLLC7gergCQ=="
	)
	raw, err := ParseSeed(seedB64)
	require.NoError(t, err)
	signer, err := NewSigner(raw)
	require.NoError(t, err)
	require.Equal(t, publicB64, base64.StdEncoding.EncodeToString(signer.PublicKey()))
	require.Equal(t, kid, signer.KeyId())
	require.Equal(t, want, signer.Sign(timestamp, []byte(body)))
	public, err := ParsePublicKey(publicB64)
	require.NoError(t, err)
	require.NoError(t, Verify(NewKeySet(public), "1767225600", want, []byte(body), time.Unix(timestamp, 0)))
}

func TestVerify(t *testing.T) {
	signer, err := NewSigner(seed(1))
	require.NoError(t, err)
	other, err := NewSigner(seed(2))
	require.NoError(t, err)
	keys := NewKeySet(signer.PublicKey())
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"deliveryId":"x"}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	header := signer.Sign(now.Unix(), body)

	require.NoError(t, Verify(keys, ts, header, body, now))
	require.NoError(t, Verify(keys, " "+ts+" ", " "+header+" ", body, now.Add(4*time.Minute)))
	for name, err := range map[string]error{
		"too late":          Verify(keys, ts, header, body, now.Add(6*time.Minute)),
		"too early":         Verify(keys, ts, header, body, now.Add(-6*time.Minute)),
		"another body":      Verify(keys, ts, header, []byte(`{"deliveryId":"y"}`), now),
		"another timestamp": Verify(keys, strconv.FormatInt(now.Unix()+1, 10), header, body, now),
		"leading zero":      Verify(keys, "0"+ts, header, body, now),
		"not a number":      Verify(keys, "soon", header, body, now),
		"another key":       Verify(keys, ts, other.Sign(now.Unix(), body), body, now),
		"unknown key id":    Verify(NewKeySet(other.PublicKey()), ts, header, body, now),
		"v1 header":         Verify(keys, ts, "v1=00", body, now),
		"bad signature":     Verify(keys, ts, "v2 kid="+signer.KeyId()+" sig=AAAA", body, now),
	} {
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	require.ErrorIs(t, Verify(keys, ts, other.Sign(now.Unix(), body), body, now), ErrUnknownKey, "a key id it does not know says so, for a fetch")
	require.NotErrorIs(t, Verify(keys, ts, "v2 kid="+signer.KeyId()+" sig=AAAA", body, now), ErrUnknownKey)
	// The key id names the key; a key under another key's id fails.
	forged := KeySet{signer.KeyId(): other.PublicKey()}
	require.ErrorIs(t, Verify(forged, ts, header, body, now), ErrInvalid)
}

func TestPublishedKeys(t *testing.T) {
	a, err := NewSigner(seed(1))
	require.NoError(t, err)
	b, err := NewSigner(seed(2))
	require.NoError(t, err)
	published := Publish(a.PublicKey(), b.PublicKey())
	set, err := published.KeySet()
	require.NoError(t, err)
	require.Len(t, set, 2)
	require.Equal(t, a.PublicKey(), set[a.KeyId()])

	wrongId := Publish(a.PublicKey())
	wrongId.Keys[0].Kid = b.KeyId()
	_, err = wrongId.KeySet()
	require.Error(t, err, "a key published under another key's id is refused")
	wrongAlg := Publish(a.PublicKey())
	wrongAlg.Keys[0].Alg = "HS256"
	_, err = wrongAlg.KeySet()
	require.Error(t, err)
}

func TestFetch(t *testing.T) {
	a, err := NewSigner(seed(1))
	require.NoError(t, err)
	serve := func(status int, body string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	published, err := json.Marshal(Publish(a.PublicKey()))
	require.NoError(t, err)
	set, err := Fetch(context.Background(), nil, serve(http.StatusOK, string(published)))
	require.NoError(t, err)
	require.Equal(t, a.PublicKey(), set[a.KeyId()])
	for name, url := range map[string]string{
		"an error page that is JSON": serve(http.StatusNotFound, `{"error":"not found"}`),
		"a server error":             serve(http.StatusInternalServerError, string(published)),
		"no keys":                    serve(http.StatusOK, `{"keys":[]}`),
		"not JSON":                   serve(http.StatusOK, `<html>`),
		"a 200 without keys":         serve(http.StatusOK, `{"error":"nope"}`),
	} {
		_, err := Fetch(context.Background(), nil, url)
		require.Error(t, err, name)
	}
}

func TestCheckEnvelope(t *testing.T) {
	const (
		audience = "pgm:7d0c0000-0000-0000-0000-000000000000:refunds.issue:0123456789abcdef"
		tenant   = "7d0c0000-0000-0000-0000-000000000000"
		id       = "00000000-0000-0000-0000-000000000001"
	)
	good := `{"version":2,"kind":"delivery","audience":"` + audience + `","tenantId":"` + tenant + `","deliveryId":"` + id + `","notAfter":"2026-11-26T13:00:00Z"}`
	e, err := CheckEnvelope([]byte(good), id, audience, tenant)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 11, 26, 13, 0, 0, 0, time.UTC), e.NotAfter)
	for name, check := range map[string]func() error{
		"another audience": func() error { _, err := CheckEnvelope([]byte(good), id, audience+"0", tenant); return err },
		"no audience set":  func() error { _, err := CheckEnvelope([]byte(good), id, "", tenant); return err },
		"another tenant":   func() error { _, err := CheckEnvelope([]byte(good), id, audience, "other"); return err },
		"another id":       func() error { _, err := CheckEnvelope([]byte(good), "other", audience, tenant); return err },
		"a callback": func() error {
			_, err := CheckEnvelope([]byte(replace(good, `"delivery"`, `"callback"`)), id, audience, tenant)
			return err
		},
		"version 1": func() error {
			_, err := CheckEnvelope([]byte(replace(good, `"version":2`, `"version":1`)), id, audience, tenant)
			return err
		},
		"no deadline": func() error {
			_, err := CheckEnvelope([]byte(replace(good, `,"notAfter":"2026-11-26T13:00:00Z"`, ``)), id, audience, tenant)
			return err
		},
		"not json": func() error { _, err := CheckEnvelope([]byte(`nope`), id, audience, tenant); return err },
	} {
		require.ErrorIs(t, check(), ErrEnvelope, name)
	}
}

func replace(s, old, new string) string { return strings.ReplaceAll(s, old, new) }
