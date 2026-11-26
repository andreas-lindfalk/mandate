package web

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A listbox role promises arrow keys, a roving focus and options that are not
// buttons. The app's own popups are plain disclosures of buttons; a real
// listbox comes from a Radix primitive, which keeps that promise itself.
func TestNoHandRolledListbox(t *testing.T) {
	forbidden := regexp.MustCompile(`role="(listbox|option)"|aria-haspopup="listbox"`)
	checked := 0
	err := fs.WalkDir(os.DirFS("src"), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tsx") || strings.HasPrefix(path, "gen/") {
			return err
		}
		data, err := fs.ReadFile(os.DirFS("src"), path)
		if err != nil {
			return err
		}
		checked++
		for i, line := range strings.Split(string(data), "\n") {
			m := forbidden.FindString(line)
			require.Empty(t, m, "%s:%d: %s without the listbox keyboard behaviour; use a disclosure of buttons", path, i+1, m)
		}
		return nil
	})
	require.NoError(t, err)
	require.Positive(t, checked)
}

// The tenant switcher marks the current tenant with aria-current, and closing
// it with Escape or by choosing a tenant puts focus back on its trigger
// instead of dropping it to the page with the unmounted button.
func TestTenantSwitcherIsADisclosureThatKeepsFocus(t *testing.T) {
	data, err := os.ReadFile("src/components/layout/AppShell.tsx")
	require.NoError(t, err)
	src := string(data)
	start := strings.Index(src, "function TenantSwitcher(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(src[start:], "\nfunction ")
	require.Positive(t, end)
	switcher := src[start : start+end]

	require.Contains(t, switcher, "aria-expanded={open}")
	require.Contains(t, switcher, "aria-current={tenant.tenantId === session.tenant?.tenantId || undefined}")
	require.Contains(t, switcher, "ref={triggerRef}", "the trigger can be focused again")
	require.Regexp(t, `const close = \(\) => \{\s*setOpen\(false\);\s*triggerRef\.current\?\.focus\(\);\s*\};`, switcher)
	require.Contains(t, switcher, `if (event.key === "Escape") close();`, "Escape returns focus to the trigger")
	require.Regexp(t, `session\.selectTenant\(tenant\.tenantId\);\s*close\(\);`, switcher, "choosing a tenant returns focus to the trigger")
}

// Switching to another tenant leaves the old tenant's page, as the tenant
// picker does: a proposal open at /inbox/<id> is not found in the new tenant,
// and per-page filters would carry over. Choosing the current tenant again
// stays where it is.
func TestTenantSwitcherLeavesTheOldTenantsPage(t *testing.T) {
	data, err := os.ReadFile("src/components/layout/AppShell.tsx")
	require.NoError(t, err)
	src := string(data)
	start := strings.Index(src, "function TenantSwitcher(")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(src[start:], "\nfunction ")
	require.Positive(t, end)
	switcher := src[start : start+end]

	require.Contains(t, switcher, "const navigate = useNavigate();")
	require.Regexp(t, `const changed = tenant\.tenantId !== session\.tenant\?\.tenantId;\s*session\.selectTenant\(tenant\.tenantId\);`, switcher,
		"whether the tenant changes is read before selectTenant replaces it")
	require.Regexp(t, `session\.selectTenant\(tenant\.tenantId\);\s*close\(\);\s*(//[^\n]*\n\s*)*if \(changed\) navigate\("/inbox"\);`, switcher,
		"a new tenant sends the user to the inbox")

	picker, err := os.ReadFile("src/pages/LoginPage.tsx")
	require.NoError(t, err)
	require.Regexp(t, `session\.selectTenant\(tenant\.tenantId\);\s*navigate\("/inbox"\);`, string(picker),
		"the switcher goes where the tenant picker goes")
}
