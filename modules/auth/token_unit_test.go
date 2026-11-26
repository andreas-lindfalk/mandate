package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Sessions and sign-in links both store the hash of a token newToken made,
// and both are looked up through tokenHash: what newToken stores is what
// tokenHash finds, and only a text of 32 to 64 bytes is hashed at all.
func TestTokenHashFindsWhatNewTokenStores(t *testing.T) {
	token, stored, err := newToken()
	require.NoError(t, err)
	hash, ok := tokenHash(token)
	require.True(t, ok)
	require.Equal(t, stored, hash)

	for n, want := range map[int]bool{0: false, 31: false, 32: true, 43: true, 64: true, 65: false} {
		hash, ok := tokenHash(strings.Repeat("a", n))
		require.Equal(t, want, ok, n)
		require.Equal(t, want, hash != nil, n)
	}
}
