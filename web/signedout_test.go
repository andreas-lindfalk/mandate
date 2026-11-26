package web

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// A session that ends while the app is open (it expires, the person signs
// out in another tab, the server stops accepting it) sends the person to
// sign in. Without it, polls fail quietly behind a stale note and an Approve
// only toasts "You are signed out" with no way back, until a window focus
// happens to ask Me again. Any query or mutation refused as Unauthenticated
// therefore invalidates Me; Me answers that state with null, so the refetch
// cannot loop, and RequireSession sends a signed-out person to /login after
// remembering the page.
func TestASessionThatEndsSendsThePersonToSignIn(t *testing.T) {
	read := func(path string) string {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		return regexp.MustCompile(`\s+`).ReplaceAllString(string(data), " ")
	}

	main := read("src/main.tsx")
	require.Contains(t, main,
		`function onError(error: unknown) { if (ConnectError.from(error).code === Code.Unauthenticated) void queryClient.invalidateQueries({ queryKey: ["me"] }); }`,
		"a call refused as Unauthenticated asks Me again, and nothing else does")
	require.Contains(t, main, "queryCache: new QueryCache({ onError }),", "a failed query or poll is caught")
	require.Contains(t, main, "mutationCache: new MutationCache({ onError }),", "a failed mutation, such as an Approve, is caught")

	session := read("src/lib/session.tsx")
	require.Contains(t, session, `queryKey: ["me", generation],`, `Me's key starts with "me", so the invalidation reaches it`)
	require.Contains(t, session,
		"if (ConnectError.from(error).code === Code.Unauthenticated) return null; throw error;",
		"Me answers a missing session with null and not an error, so the invalidation cannot loop")
	require.Contains(t, session, "signedIn: !!me.data,", "a null Me is signed out")

	app := read("src/App.tsx")
	require.Contains(t, app, `if (!session.signedIn) { remember(here); return <Navigate to="/login" replace />; }`,
		"a signed-out person is sent to sign in, and back to this page after it")
}
