package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// runJSONModule runs web/src/lib/json.ts in Node and returns what the
// script printed as JSON. It needs Node 22.6 or later, which strips the
// types; without it the test is skipped, and the static check below still
// keeps the pages off the lossy round trip.
func runJSONModule(t *testing.T, script string) map[string]any {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "--version").Output()
	require.NoError(t, err)
	m := regexp.MustCompile(`^v(\d+)\.(\d+)`).FindStringSubmatch(strings.TrimSpace(string(out)))
	require.Len(t, m, 3, "node --version printed %q", out)
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 22 || major == 22 && minor < 6 {
		t.Skipf("node %s cannot strip TypeScript types", strings.TrimSpace(string(out)))
	}
	module, err := filepath.Abs("src/lib/json.ts")
	require.NoError(t, err)
	src := `import { prettyJson, sameJson } from ` + strconv.Quote("file://"+filepath.ToSlash(module)) + ";\n" + script
	cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", "--input-type=module", "-e", src)
	cmd.Stderr = os.Stderr
	printed, err := cmd.Output()
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(printed, &result))
	return result
}

// The approval panel shows the payload exactly as the server keeps it, and
// the editor starts from that text: a number beyond a float64 is never
// rounded, so the person approves the value the receiver is sent.
func TestPrettyJSONKeepsNumbersAsWritten(t *testing.T) {
	result := runJSONModule(t, `
const payload = '{"amount": 9007199254740993, "big": 1e400, "id": 12345678901234567890, "e": 1e21, "neg": -0.10, "s": "a, b: {c} [d] \\"e\\" \\\\", "empty": {}, "none": [ ], "list": [1, {"x": true}, null]}';
console.log(JSON.stringify({
  pretty: prettyJson(payload),
  plain: prettyJson('{"a":[1,2,{"b":"c"}],"d":{},"e":[]}'),
  plainNative: JSON.stringify(JSON.parse('{"a":[1,2,{"b":"c"}],"d":{},"e":[]}'), null, 2),
  scalar: prettyJson(' 42 '),
  invalid: prettyJson('{"a": 1'),
  sameSpacing: sameJson('{"a": 9007199254740993}', '{\n  "a":9007199254740993\n}'),
  sameRounded: sameJson('{"a": 9007199254740993}', '{"a": 9007199254740992}'),
  sameInvalid: sameJson('{"a": 1', '{"a": 1'),
}));
`)
	require.Equal(t, `{
  "amount": 9007199254740993,
  "big": 1e400,
  "id": 12345678901234567890,
  "e": 1e21,
  "neg": -0.10,
  "s": "a, b: {c} [d] \"e\" \\",
  "empty": {},
  "none": [],
  "list": [
    1,
    {
      "x": true
    },
    null
  ]
}`, result["pretty"])
	require.Equal(t, result["plainNative"], result["plain"], "without large numbers the layout is the one JSON.stringify gives")
	require.Equal(t, "42", result["scalar"])
	require.Equal(t, `{"a": 1`, result["invalid"])
	require.Equal(t, true, result["sameSpacing"])
	require.Equal(t, false, result["sameRounded"], "a rounded number is a different payload, so the approval sends it as an edit")
	require.Equal(t, false, result["sameInvalid"])
}

// No page formats JSON from the server by parsing it into JS values, which
// rounds its numbers; they go through prettyJson and sameJson instead.
func TestPagesNeverRoundTripJSON(t *testing.T) {
	files, err := filepath.Glob("src/pages/*.tsx")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	lossy := regexp.MustCompile(`JSON\.stringify\(\s*JSON\.parse\(`)
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		require.False(t, lossy.Match(data), "%s formats JSON with JSON.stringify(JSON.parse(...)), which rounds numbers; use prettyJson", file)
	}
}
