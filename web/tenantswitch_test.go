package web

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A tenant the session picks without the person choosing (the only one they
// have, or the only one left after a removal) is kept as the chosen one, so a
// second tenant added later does not send them to the picker mid-review. A
// tenant that replaces another whose data is in the cache clears that data
// and leaves the old tenant's page, as a switch by hand does.
func TestAnImplicitTenantIsKeptAndClearsTheOldOne(t *testing.T) {
	data, err := os.ReadFile("src/lib/session.tsx")
	require.NoError(t, err)
	src := regexp.MustCompile(`\s+`).ReplaceAllString(string(data), " ")

	require.True(t, strings.Contains(src, "tenants.find((t) => t.tenantId === (tenantId ?? sessionStore.tenantId))"),
		"the tenant is resolved against the remembered one as well, not only the one chosen in this page's state")

	require.True(t, strings.Contains(src, "if (resolved && resolved !== tenantId) setTenantId(resolved);"),
		"the implicit tenant becomes the chosen one")

	start := strings.Index(src, "React.useEffect(() => { if (!resolved || cachedTenant.current === resolved) return;")
	require.GreaterOrEqual(t, start, 0, "an effect keyed on the resolved tenant clears what another tenant left in the cache")
	end := strings.Index(src[start:], "}, [resolved, queryClient, navigate]);")
	require.Positive(t, end, "the effect runs when the resolved tenant changes")
	effect := src[start : start+end]
	require.Contains(t, effect, "const switched = cachedTenant.current !== null;", "a first tenant after sign-in is no switch, so a returning link keeps its place")
	require.Regexp(t, `if \(!switched\) return; removeTenantQueries\(queryClient\); navigate\("/inbox", \{ replace: true \}\);`, effect,
		"a switch away from a cached tenant clears its data and leaves its page")

	require.True(t, strings.Contains(src, "const cachedTenant = React.useRef<string | null>(null);"), "nothing is cached for a tenant before one is resolved")
	require.True(t, strings.Contains(src, "selectTenant(id) { sessionStore.setTenant(id); cachedTenant.current = id; setTenantId(id); removeTenantQueries(queryClient); }"),
		"a switch by hand records the tenant now cached, so the effect does not clear it again")
	require.Equal(t, 3, strings.Count(src, "sessionStore.setTenant(null); cachedTenant.current = null; setTenantId(null);"),
		"sign-in by token, sign-in by cookie and sign-out empty the cache and forget its tenant")
	require.Equal(t, 1, strings.Count(src, "queryClient.removeQueries("), "the tenant-scoped queries are cleared in one place")
}
