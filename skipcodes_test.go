package pgmandate

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every code the server gives for why a proposal waits for a person is
// written down where a client reads it: on skipped in the agent contract, on
// skip_reason in the people's API, in the generated code of both, and in
// docs/agents.md. The codes are read from the modules' constants, so a new
// one that is not documented fails here. The refused_ prefix is documented
// as a prefix, and every place says that codes may be added.
func TestEverySkipCodeIsDocumented(t *testing.T) {
	written := regexp.MustCompile(`\bSkip[A-Za-z]+\s*=\s*"([a-z_]+)"`)
	var codes []string
	prefixes := 0
	for _, path := range []string{"modules/proposals/proposals.go", "modules/mandates/mandates.go"} {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, m := range written.FindAllStringSubmatch(string(data), -1) {
			if strings.HasSuffix(m[1], "_") {
				prefixes++
				require.Equal(t, SkipRefused, m[1], "%s writes the prefix %q", path, m[1])
				continue
			}
			codes = append(codes, m[1])
		}
	}
	require.Equal(t, 1, prefixes)
	for _, code := range []string{SkipNotExecutable, SkipPayloadInvalid, SkipActionChanged, SkipFlagged,
		SkipNoMandate, SkipRiskAbove, SkipBudgetSpent, SkipConditionUnmet, SkipAgentRevoked,
		SkipActionFailed, SkipExecutionFailed, SkipActionRemoved} {
		require.Contains(t, codes, code)
	}
	require.Len(t, codes, 12)

	fields := map[string]string{
		"proto/mandate/agent/v1/agent.proto":       "string skipped = 14;",
		"proto/mandate/v1/proposals.proto":         "string skip_reason = 18;",
		"gen/go/mandate/agent/v1/agent.pb.go":      "Skipped string `protobuf:",
		"gen/go/mandate/v1/proposals.pb.go":        "SkipReason string ",
		"web/src/gen/mandate/agent/v1/agent_pb.ts": "skipped: string;",
		"web/src/gen/mandate/v1/proposals_pb.ts":   "skipReason: string;",
	}
	for path, field := range fields {
		comment := fieldComment(t, path, field)
		for _, code := range codes {
			require.Regexp(t, `\b`+code+`\b`, comment, "%s does not name %q on %s", path, code, field)
		}
		require.Contains(t, comment, SkipRefused+" followed by", path)
		require.Contains(t, comment, "Codes may be added", path)
		require.Contains(t, comment, "what the last execution attempt recorded, or what rules out executing it", path)
	}

	data, err := os.ReadFile("docs/agents.md")
	require.NoError(t, err)
	doc := string(data)
	for _, code := range codes {
		require.Contains(t, doc, "| `"+code+"` |", "docs/agents.md has no row for %q", code)
	}
	require.Contains(t, doc, "| `"+SkipRefused+"<code>` |")
	require.Contains(t, doc, "Codes may be added")
}

// fieldComment is the comment right above the one line in the file that
// holds field, joined into one line with the comment markers removed.
func fieldComment(t *testing.T, path, field string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(string(data), "\n")
	at := -1
	for i, line := range lines {
		if strings.Contains(strings.Join(strings.Fields(line), " "), field) {
			require.Equal(t, -1, at, "%s holds %q more than once", path, field)
			at = i
		}
	}
	require.NotEqual(t, -1, at, "%s does not hold %q", path, field)
	var comment []string
	for i := at - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(line, "//"):
			comment = append([]string{strings.TrimSpace(strings.TrimPrefix(line, "//"))}, comment...)
		case strings.HasPrefix(line, "*") && !strings.HasPrefix(line, "*/"):
			comment = append([]string{strings.TrimSpace(strings.TrimPrefix(line, "*"))}, comment...)
		case line == "/**" || line == "*/":
		default:
			require.NotEmpty(t, comment, "%s has no comment on %q", path, field)
			return strings.Join(comment, " ")
		}
	}
	return strings.Join(comment, " ")
}
