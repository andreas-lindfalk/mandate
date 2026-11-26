package web

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Evidence table links an event to /inbox/<subject>, which only works
// when the subject is a proposal. proposal.rapid_approvals has the person who
// approved as its subject, so a prefix match on "proposal." opened the inbox
// on a person id and showed "It no longer exists." The table links from an
// explicit list, proposalEvents, and this test holds that list to what the
// server writes.
func TestTheEvidenceTableLinksOnlyProposalSubjects(t *testing.T) {
	data, err := os.ReadFile("src/lib/format.ts")
	require.NoError(t, err)
	src := string(data)

	start := strings.Index(src, "export const proposalEvents = new Set([")
	require.GreaterOrEqual(t, start, 0, "web/src/lib/format.ts has no proposalEvents set")
	end := strings.Index(src[start:], "\n]);")
	require.Positive(t, end)
	linked := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z_]+\.[a-z_]+)"`).FindAllStringSubmatch(src[start:start+end], -1) {
		linked[m[1]] = true
	}
	require.NotEmpty(t, linked)

	start = strings.Index(src, "export const eventLabel")
	require.GreaterOrEqual(t, start, 0)
	end = strings.Index(src[start:], "\n};")
	require.Positive(t, end)
	labels := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z_]+\.[a-z_]+)":`).FindAllStringSubmatch(src[start:start+end], -1) {
		labels[m[1]] = true
	}

	// The proposal and delivery events whose subject is not a proposal.
	notAboutAProposal := map[string]bool{"proposal.rapid_approvals": true}
	for event := range linked {
		require.True(t, labels[event], "proposalEvents has %q, which has no entry in eventLabel", event)
		require.False(t, notAboutAProposal[event], "proposalEvents has %q, whose subject is not a proposal", event)
	}
	// A new proposal or delivery event needs a decision: link it or not.
	for event := range labels {
		if strings.HasPrefix(event, "proposal.") || strings.HasPrefix(event, "delivery.") {
			require.True(t, linked[event] || notAboutAProposal[event], "%q is neither in proposalEvents nor known not to be about a proposal", event)
		}
	}

	// What the server writes as the subject of each event it names. The
	// expressions in proposalSubjects hold a proposal's id where they are used.
	proposalSubjects := map[string]bool{"id": true, "ids[0]": true, "proposalId": true, "delivery.ProposalId": true}
	written := regexp.MustCompile(`Type: +"([a-z_]+\.[a-z_]+)",\s*SubjectId: +([A-Za-z_][A-Za-z0-9_.]*(?:\[\d+\])?)`)
	root := os.DirFS("..")
	seen := map[string]string{}
	err = fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "gen", "node_modules", "web", ".git":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		for _, m := range written.FindAllStringSubmatch(string(data), -1) {
			event, subject := m[1], m[2]
			seen[event] = subject
			if linked[event] {
				require.True(t, proposalSubjects[subject], "%s writes %q with the subject %s, which is not known to be a proposal, yet the Evidence table links it to the inbox", path, event, subject)
			}
			if notAboutAProposal[event] {
				require.False(t, proposalSubjects[subject], "%s writes %q with a proposal as its subject; link it from proposalEvents", path, event)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "personId", seen["proposal.rapid_approvals"], "the server writes proposal.rapid_approvals about the person who approved")
	require.Equal(t, "proposalId", seen["delivery.retargeted"])

	page, err := os.ReadFile("src/pages/EvidencePage.tsx")
	require.NoError(t, err)
	require.Contains(t, string(page), "{proposalEvents.has(e.type) ? (\n", "the Evidence table links an event to the inbox by proposalEvents")
	require.NotRegexp(t, `startsWith\("(proposal|delivery)\.`, string(page), "the Evidence table links by a prefix, which takes in events about a person")
}
