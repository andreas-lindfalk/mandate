// Package injection is a tripwire for the most obvious text that tries to
// steer a language model, such as "ignore previous instructions" or a line
// posing as a system message. It is a handful of English patterns: it will
// miss a paraphrase, another language or an encoding, and it will now and
// then flag innocent text. It is not a control. The controls are the
// action's schema and risk, the mandate and the person. A match flags the
// proposal, and a flagged proposal is high risk and waits for a person.
package injection

import (
	"regexp"
	"slices"
	"strings"
)

// Signals are what was found, as stable keys.
const (
	SignalOverride = "override"      // "ignore previous instructions" and the like
	SignalRole     = "role"          // a line posing as a system or assistant message
	SignalMarkup   = "markup"        // tags that could close a prompt's data block
	SignalAddress  = "addressing_ai" // text addressed to an AI, model or the system
)

var patterns = []struct {
	signal string
	re     *regexp.Regexp
}{
	{SignalOverride, regexp.MustCompile(`(?is)\b(ignore|disregard|forget|override)\b.{0,40}\b(instructions?|rules|guidelines|above|previous|prior|policy|mandate)\b`)},
	{SignalOverride, regexp.MustCompile(`(?i)\bnew (instructions?|rules|system prompt)\b`)},
	{SignalRole, regexp.MustCompile(`(?im)^\s*(system|assistant|developer|tool)\s*:`)},
	{SignalMarkup, regexp.MustCompile(`(?i)(<|‹|＜)\s*/?\s*(system|user|assistant|tool|instructions?|payload|data|document)\b`)},
	{SignalAddress, regexp.MustCompile(`(?is)\b(to|for|dear)\s+(the\s+)?(ai|agent|model|assistant|llm|system)\b.{0,30}\b(should|must|shall|need to|have to)\b`)},
	{SignalAddress, regexp.MustCompile(`(?i)\b(you are now|act as|pretend to be|from now on you)\b`)},
}

// invisible removes characters that split a word without showing.
var invisible = strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "", "\u00ad", "")

// Signals returns the kinds of steering text found in the texts, sorted and
// without duplicates; nil when none.
func Signals(texts ...string) []string {
	var found []string
	for _, text := range texts {
		text = invisible.Replace(text)
		for _, p := range patterns {
			if !slices.Contains(found, p.signal) && p.re.MatchString(text) {
				found = append(found, p.signal)
			}
		}
	}
	slices.Sort(found)
	return found
}
