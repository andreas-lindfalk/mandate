package web

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A two-step confirmation must not be passed by a double-click. The first
// click arms it, and the second lands where the trigger was: so the trigger's
// place goes to Cancel, which comes last in the right-aligned group, and the
// danger button to its left. The danger button also drops the second click
// of a double-click (singleClick), in case the trigger was wider than Cancel.
// The danger button is never the trigger relabelled. Arming the confirmation
// unmounts the trigger that had focus, so Cancel takes focus (never the danger
// button, or a second Enter would carry it out), and closing it gives focus
// back to the trigger through useFocusBack on the same state.
func TestConfirmationsSurviveADoubleClick(t *testing.T) {
	space := regexp.MustCompile(`\s+`)
	dangerAt := regexp.MustCompile(`<Button variant="danger" size="sm"`)
	confirm := regexp.MustCompile(`^<Button variant="danger" size="sm" onClick=\{singleClick\(\(\) => [\w.]+\([\w.]*\)\)\} loading=\{[^}]+\} ?> [^<]+ </Button> <Button variant="ghost" size="sm" autoFocus onClick=\{\(\) => set(?P<cancels>\w+)\((false|null)\)\}> Cancel </Button> (</div> |</> )\) : \( <Button variant="\w+" size="sm" (className="[^"]*" )?ref=\{(?P<ref>\w+)\((?P<key>[\w.]*)\)\} onClick=\{\(\) => set(?P<arms>\w+)\((true|(?P<armedKey>token\.id))\)\}>`)
	cancelFirst := regexp.MustCompile(`Cancel </Button> <Button variant="danger"`)

	confirms := 0
	err := fs.WalkDir(os.DirFS("src/pages"), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tsx") {
			return err
		}
		data, err := fs.ReadFile(os.DirFS("src/pages"), path)
		if err != nil {
			return err
		}
		src := space.ReplaceAllString(string(data), " ")
		require.NotRegexp(t, cancelFirst, src, "%s: Cancel before the danger button puts the danger button where the trigger was", path)
		for _, at := range dangerAt.FindAllStringIndex(src, -1) {
			confirms++
			end := min(at[0]+700, len(src))
			m := confirm.FindStringSubmatch(src[at[0]:end])
			require.NotNil(t, m, "%s: a confirming danger button drops a double-click's second click, has Cancel after it, Cancel takes focus and the trigger gets it back: %s", path, src[at[0]:end])
			group := func(name string) string { return m[confirm.SubexpIndex(name)] }
			require.Equal(t, group("arms"), group("cancels"), "%s: Cancel clears the state the trigger arms", path)
			state := strings.ToLower(group("arms")[:1]) + group("arms")[1:]
			require.Contains(t, src, "const "+group("ref")+" = useFocusBack("+state+");", "%s: the trigger's ref follows the state it arms", path)
			require.Equal(t, group("armedKey"), group("key"), "%s: a keyed trigger's ref has the key it arms", path)
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 6, confirms, "the revokes of an agent, a token and a mandate, and the removals of an action and a person and turning off the notifier")

	utils, err := os.ReadFile("src/lib/utils.ts")
	require.NoError(t, err)
	require.Contains(t, string(utils), "if (event.detail <= 1) run();", "a key press (detail 0) and a single click go through, a double-click's second click does not")

	agents, err := os.ReadFile("src/pages/AgentsPage.tsx")
	require.NoError(t, err)
	page := space.ReplaceAllString(string(agents), " ")
	require.NotContains(t, page, "revoking === token.id ? revokeToken.mutate", "the token's revoke is not its trigger relabelled")
	require.Contains(t, page, "loading={revokeToken.isPending && revokeToken.variables === token.id}", "a token revoke in flight cannot be sent again")
	require.Contains(t, page, `onSettled: () => setRevoking(null),`, "the confirmation closes once the revoke is done")
}
