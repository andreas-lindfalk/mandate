package web

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Evidence page shows the auditor the command that verifies an exported
// chain without this server. The command must name the file the export saves,
// or running it as shown fails with file not found. So the download and the
// command read one chainFile, and neither spells the name out on its own.
func TestTheVerifyCommandNamesTheFileTheChainExportSaves(t *testing.T) {
	data, err := os.ReadFile("src/pages/EvidencePage.tsx")
	require.NoError(t, err)
	src := string(data)

	decl := regexp.MustCompile("const chainFile = `\\$\\{prefix\\}-chain\\.jsonl`;").FindStringIndex(src)
	require.NotNil(t, decl, "EvidencePage.tsx has no chainFile name for the chain export")
	exportIt := regexp.MustCompile(`async function exportIt\(`).FindStringIndex(src)
	require.NotNil(t, exportIt)
	require.Less(t, decl[0], exportIt[0], "chainFile is worked out outside exportIt, where the command can read it")

	require.Contains(t, src, "download(chainFile, response.jsonl);", "the chain export saves chainFile")
	require.Regexp(t, `pgmandate verify-export -tenant \{session\.tenant\?\.tenantId\} -file \{chainFile\}</code>`, src,
		"the verify command names chainFile")
	require.NotRegexp(t, `-file [\w.-]*chain\.jsonl`, src, "the verify command spells out a file name of its own")
	require.Len(t, regexp.MustCompile(`-chain\.jsonl`).FindAllString(src, -1), 1, "the chain file name is written once")
}
