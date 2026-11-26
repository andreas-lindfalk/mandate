package web

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func readSource(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return regexp.MustCompile(`\s+`).ReplaceAllString(string(data), " ")
}

// useFocusBack gives focus back to the trigger of what just closed, found by
// the key that was open, and only when focus fell to the page with the
// unmounted Cancel: focus that has moved on, as to another armed
// confirmation's Cancel, stays where it is.
func TestUseFocusBackReturnsFocusOnlyWhenItWasDropped(t *testing.T) {
	src := readSource(t, "src/lib/utils.ts")
	start := strings.Index(src, "export function useFocusBack(open: string | boolean | null) {")
	require.GreaterOrEqual(t, start, 0)
	hook := src[start:]

	require.Contains(t, hook, "const closed = previous.current; previous.current = open;", "the key that was open is remembered across renders")
	require.Contains(t, hook, "if (closed === null || closed === false || closed === open) return;", "nothing closed, nothing to give back")
	require.Contains(t, hook, "if (active && active !== document.body) return;", "focus that has moved on is not taken back")
	require.Contains(t, hook, "triggers.current.get(String(closed))?.focus();", "the trigger of what closed gets focus")
	require.Contains(t, hook, "}, [open]);")
	require.Contains(t, hook, "if (element) triggers.current.set(String(key), element); else triggers.current.delete(String(key));",
		"an unmounted trigger is forgotten")
}

// Opening the limits editor unmounts Change: the first field takes focus.
// Closing it, by Cancel or a save, gives focus back to Change.
func TestLimitsEditorMovesFocusInAndBack(t *testing.T) {
	src := readSource(t, "src/pages/AgentsPage.tsx")
	require.Contains(t, src, "const limitsTrigger = useFocusBack(editingLimits);")
	require.Contains(t, src, "<Input id={`per-minute-${agent.id}`} autoFocus ", "the first field takes focus when the editor opens")
	require.Regexp(t, `<Button variant="ghost" size="sm" className="ml-1" ref=\{limitsTrigger\(\)\} onClick=\{\(\) => \{ setPerMinute\(String\(agent\.maxPerMinute\)\); setPerHour\(String\(agent\.maxPerHour\)\); setEditingLimits\(true\); \}\} > Change </Button>`, src,
		"Change gets focus back when the editor closes")
	require.Regexp(t, `onSuccess: \(\) => \{ queryClient\.invalidateQueries\(\{ queryKey: \["agents"\] \}\); setEditingLimits\(false\);`, src,
		"a save closes the editor, which gives focus back to Change")
	require.Contains(t, src, `<Button type="button" variant="ghost" onClick={() => setEditingLimits(false)}> Cancel </Button>`)
}

// "I have saved it" unmounts the token card along with itself. Focus goes to
// the Issue button the token came from, before the card goes, so it is never
// dropped to the page.
func TestClosingTheNewTokenGivesFocusToItsIssueButton(t *testing.T) {
	src := readSource(t, "src/pages/AgentsPage.tsx")
	require.Contains(t, src, "const issueButton = React.useRef<HTMLButtonElement>(null);")
	require.Contains(t, src, `<Button variant="secondary" ref={issueButton} onClick={() => issue.mutate()} loading={issue.isPending}> <KeyRound /> Issue a token </Button>`)
	require.Contains(t, src, "onToken(response.secret, () => issueButton.current?.focus());")
	require.Contains(t, src, "onToken={(issued, focusBack) => { tokenFocusBack.current = focusBack; setSecret(issued); }}")
	require.Contains(t, src, "onDone={() => { tokenFocusBack.current?.(); setSecret(null); }}", "focus moves before the card unmounts")
}

// "Show the flagged text" unmounts itself; the text it reveals takes focus,
// so a screen reader reads what was revealed.
func TestRevealingTheFlaggedTextFocusesIt(t *testing.T) {
	src := readSource(t, "src/pages/InboxPage.tsx")
	require.Contains(t, src, "const text = React.useRef<HTMLParagraphElement>(null); React.useEffect(() => { if (revealed) text.current?.focus(); }, [revealed]);")
	require.Contains(t, src, `<p id="account-text" ref={text} tabIndex={-1} `, "the revealed text can take focus")
	require.Contains(t, src, "onClick={() => setRevealed(true)}> Show the flagged text </Button>")
}
