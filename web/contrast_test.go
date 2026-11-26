package web

import (
	"io/fs"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The design tokens promise that danger-ink is readable text on the page and
// on danger-soft, and danger is not (it is for icons and borders). An error
// message a person must read uses danger-ink.
func TestDangerInkIsReadableAndDangerIsNot(t *testing.T) {
	tokens := readColorTokens(t)
	for _, bg := range []string{"background", "surface", "danger-soft"} {
		require.GreaterOrEqual(t, contrast(t, tokens["danger-ink"], tokens[bg]), 4.5, "danger-ink on %s", bg)
	}
	require.Less(t, contrast(t, tokens["danger"], tokens["background"]), 4.5, "danger is below AA for small text on the page")
}

// Every role="alert" element whose class list is on the same line is checked:
// bare text-danger is below 4.5:1 on the page background.
func TestAlertTextUsesDangerInk(t *testing.T) {
	className := regexp.MustCompile(`className="([^"]*)"`)
	checked := 0
	err := fs.WalkDir(os.DirFS("src"), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tsx") {
			return err
		}
		data, err := fs.ReadFile(os.DirFS("src"), path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, `role="alert"`) {
				continue
			}
			m := className.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			checked++
			for _, class := range strings.Fields(m[1]) {
				require.NotEqual(t, "text-danger", class, "%s:%d: alert text needs text-danger-ink for 4.5:1", path, i+1)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, checked, 3, "the sign-in, setup and sign-in link errors are checked")
}

func readColorTokens(t *testing.T) map[string]string {
	t.Helper()
	css, err := os.ReadFile("src/index.css")
	require.NoError(t, err)
	tokens := map[string]string{}
	for _, m := range regexp.MustCompile(`--color-([a-z0-9-]+):\s*(#[0-9a-fA-F]{6});`).FindAllStringSubmatch(string(css), -1) {
		tokens[m[1]] = m[2]
	}
	return tokens
}

// contrast is the WCAG 2 contrast ratio of two #rrggbb colors.
func contrast(t *testing.T, a, b string) float64 {
	t.Helper()
	la, lb := luminance(t, a), luminance(t, b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	require.Len(t, hex, 7, "a #rrggbb color, got %q", hex)
	var rgb [3]float64
	for i := range rgb {
		v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		require.NoError(t, err)
		c := float64(v) / 255
		if c <= 0.04045 {
			rgb[i] = c / 12.92
		} else {
			rgb[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
}

// Focus is ringed in accent, which is under the 3:1 a focus indicator needs
// (WCAG 1.4.11) on espresso. On the dark surfaces that hold the navigation,
// the ring is sand, and those surfaces carry the class the rule matches.
func TestFocusRingIsVisibleOnDarkSurfaces(t *testing.T) {
	tokens := readColorTokens(t)
	for _, bg := range []string{"background", "surface"} {
		require.GreaterOrEqual(t, contrast(t, tokens["accent"], tokens[bg]), 3.0, "accent ring on %s", bg)
	}
	for _, bg := range []string{"espresso", "espresso-2"} {
		require.Less(t, contrast(t, tokens["accent"], tokens[bg]), 3.0, "accent ring on %s is why sand is needed", bg)
		require.GreaterOrEqual(t, contrast(t, tokens["sand"], tokens[bg]), 3.0, "sand ring on %s", bg)
	}

	css := readSource(t, "src/index.css")
	require.Contains(t, css, ":focus-visible { outline: 2px solid var(--color-accent); outline-offset: 2px; }")
	require.Contains(t, css, `[class~="bg-espresso"] :focus-visible, [class~="bg-espresso-2"] :focus-visible { outline-color: var(--color-sand); }`)

	shell := readSource(t, "src/components/layout/AppShell.tsx")
	require.Regexp(t, `<aside className="[^"]*\bbg-espresso\b[^"]*"> <SidebarContent />`, shell, "the sidebar")
	require.Regexp(t, `<header className="[^"]*\bbg-espresso\b[^"]*"> <Wordmark /> <DialogPrimitive.Trigger`, shell, "the mobile header")
	require.Regexp(t, `<DialogPrimitive.Content aria-describedby=\{undefined\} className="[^"]*\bbg-espresso\b[^"]*"> <DialogPrimitive.Title`, shell, "the menu drawer")
	require.Regexp(t, `<ul className="[^"]*\bbg-espresso-2\b[^"]*"> \{session.tenants.map`, shell, "the tenant list")
}
