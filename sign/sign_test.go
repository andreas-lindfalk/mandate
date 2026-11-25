package sign

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSignatureRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"deliveryId":"x"}`)
	signature := Signature(secret, now.Unix(), body)
	require.True(t, len(signature) == len("v1=")+64)
	ts := strconv.FormatInt(now.Unix(), 10)

	require.NoError(t, Verify(secret, ts, signature, body, now))
	require.NoError(t, Verify(secret, ts, " "+signature+" ", body, now.Add(4*time.Minute)))
	require.ErrorIs(t, Verify(secret, ts, signature, body, now.Add(6*time.Minute)), ErrInvalid)
	require.ErrorIs(t, Verify(secret, ts, signature, body, now.Add(-6*time.Minute)), ErrInvalid)
	require.ErrorIs(t, Verify(secret, ts, signature, []byte(`{"deliveryId":"y"}`), now), ErrInvalid)
	require.ErrorIs(t, Verify([]byte("other-secret-other-secret-other!"), ts, signature, body, now), ErrInvalid)
	require.ErrorIs(t, Verify(secret, "soon", signature, body, now), ErrInvalid)
	require.ErrorIs(t, Verify(secret, ts, "v1=00", body, now), ErrInvalid)
}

// TestVector is the example in the README, so a receiver written in any
// language can check its own signing against a known answer.
func TestVector(t *testing.T) {
	const (
		secret    = "mws_example_secret_0123456789abcdef"
		timestamp = int64(1767225600)
		body      = `{"version":1,"deliveryId":"00000000-0000-0000-0000-000000000001"}`
		want      = "v1=7d1b37c2f39b25a83f48808b5326479a4ad2265ee5b7fc034352bb047355f0d9"
	)
	require.Equal(t, want, Signature([]byte(secret), timestamp, []byte(body)))
	require.NoError(t, Verify([]byte(secret), "1767225600", want, []byte(body), time.Unix(timestamp, 0)))
}
