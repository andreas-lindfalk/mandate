package events_test

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/testkit"
)

// The log holds digests of what the service wrote, and the database holds
// the values as jsonb. A digest must come out the same before and after:
// what Go writes in canonical form is what jsonb gives back. The tests in
// canonical_test.go say so for cases written by hand. These say it for
// numbers and documents made from a seed, against a real Postgres.
//
// A longer hunt:
//
//	MANDATE_RANDOM_CASES=1000000 MANDATE_RANDOM_SEED=random go test -timeout 30m \
//	    -run 'TestJsonb' ./pkg/events/

const (
	defaultNumberCases   = 20_000
	defaultDocumentCases = 5_000
	jsonbBatch           = 500
)

func startJsonb(t *testing.T) *pgx.Conn {
	t.Helper()
	if testing.Short() {
		t.Skip("starts Postgres")
	}
	ctx := context.Background()
	pg, superuser, err := testkit.StartBarePostgres(ctx)
	require.NoError(t, err)
	t.Cleanup(pg.Close)
	conn, err := pgx.Connect(ctx, superuser)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })
	return conn
}

// jsonbKeeps asks Postgres, for each text, whether it reads as jsonb and
// what it gives back.
func jsonbKeeps(t *testing.T, conn *pgx.Conn, texts []string) (valid []bool, kept []string) {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT pg_input_is_valid(t.text, 'jsonb'), CASE WHEN pg_input_is_valid(t.text, 'jsonb') THEN (t.text::jsonb)::text ELSE '' END
		FROM unnest($1::text[]) WITH ORDINALITY AS t(text, i) ORDER BY t.i`, texts)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var ok bool
		var text string
		require.NoError(t, rows.Scan(&ok, &text))
		valid, kept = append(valid, ok), append(kept, text)
	}
	require.NoError(t, rows.Err())
	require.Len(t, valid, len(texts))
	return valid, kept
}

func canonicalOf(t *testing.T, text string) string {
	t.Helper()
	out, err := events.Canonical(json.RawMessage(text))
	require.NoError(t, err, text)
	return string(out)
}

// shown cuts a long text, such as a number of a hundred thousand digits,
// for a failure message.
func shown(text string) string {
	if len(text) <= 300 {
		return text
	}
	return text[:150] + " ... (" + strconv.Itoa(len(text)) + " bytes) ... " + text[len(text)-100:]
}

// A number the service says Postgres can store, Postgres stores, and gives
// back spelled exactly as the canonical form spells it. A number it says
// Postgres cannot store is left as written.
func TestJsonbKeepsTheNumbersTheServiceSaysFit(t *testing.T) {
	conn := startJsonb(t)
	cases, seed := testkit.RandomRun(t, defaultNumberCases)
	g := numbers{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // a test's generator
	var fit, unfit, unfitButKept int
	for done := 0; done < cases; done += jsonbBatch {
		batch := make([]string, min(jsonbBatch, cases-done))
		for i := range batch {
			batch[i] = g.number()
		}
		valid, kept := jsonbKeeps(t, conn, batch)
		for i, n := range batch {
			if !events.NumberFits(n) {
				unfit++
				if valid[i] {
					unfitButKept++
				}
				require.Equal(t, n, canonicalOf(t, n), "a number that does not fit is left as written (seed %d)", seed)
				require.Equal(t, -1, events.PlainLength(n), "a number that does not fit has no length (seed %d): %s", seed, shown(n))
				continue
			}
			fit++
			require.True(t, valid[i], "the service says Postgres can store a number it refuses (seed %d): %s", seed, shown(n))
			written := canonicalOf(t, n)
			require.Equal(t, shown(kept[i]), shown(written), "jsonb spells a number another way than the canonical form (seed %d): %s", seed, shown(n))
			require.Equal(t, kept[i], written, "jsonb spells a number another way than the canonical form (seed %d): %s", seed, shown(n))
			require.LessOrEqual(t, len(written), events.PlainLength(n), "the canonical form is longer than the length foretold (seed %d): %s", seed, shown(n))
		}
	}
	t.Logf("%d numbers fit and came back as written; %d do not fit, of which Postgres would still take %d", fit, unfit, unfitButKept)
	// A short run is too short to ask that both kinds have their share.
	if cases >= 2_000 {
		require.Greater(t, fit, cases/4, "too few numbers that fit")
		require.Greater(t, unfit, cases/100, "too few numbers that do not fit")
	}
}

// A document's canonical form, and so its digest, is the same before and
// after jsonb: whatever the order of its keys, its spacing, a key written
// twice, or the spelling of its numbers.
func TestJsonbKeepsADocumentsDigest(t *testing.T) {
	conn := startJsonb(t)
	cases, seed := testkit.RandomRun(t, defaultDocumentCases)
	g := numbers{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // a test's generator
	for done := 0; done < cases; done += jsonbBatch {
		batch := make([]string, min(jsonbBatch, cases-done))
		for i := range batch {
			batch[i] = g.document(3)
		}
		valid, kept := jsonbKeeps(t, conn, batch)
		for i, doc := range batch {
			require.True(t, valid[i], "jsonb refuses a document the generator promised it takes (seed %d): %s", seed, shown(doc))
			before, after := canonicalOf(t, doc), canonicalOf(t, kept[i])
			require.Equal(t, shown(before), shown(after), "the canonical form changes through jsonb (seed %d): %s", seed, shown(doc))
			require.Equal(t, before, after, "the canonical form changes through jsonb (seed %d): %s", seed, shown(doc))
			require.Equal(t, before, canonicalOf(t, before), "the canonical form is not stable (seed %d): %s", seed, shown(doc))
			digestBefore, err := events.Digest(json.RawMessage(doc))
			require.NoError(t, err)
			digestAfter, err := events.Digest(json.RawMessage(kept[i]))
			require.NoError(t, err)
			require.Equal(t, digestBefore, digestAfter, "the digest changes through jsonb (seed %d): %s", seed, shown(doc))
		}
	}
}

// numbers writes JSON numbers and documents from a seed.
type numbers struct {
	r *rand.Rand
}

func (g *numbers) chance(percent int) bool {
	return g.r.IntN(100) < percent
}

func (g *numbers) digits(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('0' + g.r.IntN(10))
	}
	return string(b)
}

// lengths around the most digits Postgres's numeric keeps before and after
// the point, and exponents that move a number across those bounds.
var (
	wholeLengths    = []int{1, 1, 1, 2, 3, 5, 10, 20, 40}
	wholeExtremes   = []int{1000, events.MaxIntegerDigits - 1, events.MaxIntegerDigits, events.MaxIntegerDigits + 1}
	decimalLengths  = []int{1, 1, 2, 3, 5, 10, 20, 40}
	decimalExtremes = []int{1000, events.MaxDecimalDigits - 1, events.MaxDecimalDigits, events.MaxDecimalDigits + 1}
	exponents       = []int{0, 1, 2, 3, 5, 7, 10, 20, 30, 100, 300}
	exponentExtrema = []int{1000, events.MaxDecimalDigits - 1, events.MaxDecimalDigits, events.MaxDecimalDigits + 1,
		events.MaxIntegerDigits - 1, events.MaxIntegerDigits, events.MaxIntegerDigits + 1,
		events.MaxIntegerDigits + events.MaxDecimalDigits, events.MaxIntegerDigits + events.MaxDecimalDigits + 1,
		1_000_000, 999_999_999}
)

func pickInt(g *numbers, usual, extreme []int, extremePercent int) int {
	if g.chance(extremePercent) {
		return extreme[g.r.IntN(len(extreme))]
	}
	return usual[g.r.IntN(len(usual))]
}

// number writes a JSON number: any sign, zeros where they change nothing,
// and now and then a length or an exponent at the edge of what Postgres
// keeps.
func (g *numbers) number() string {
	var b strings.Builder
	if g.chance(30) {
		b.WriteByte('-')
	}
	switch {
	case g.chance(25):
		b.WriteByte('0')
	default:
		b.WriteByte(byte('1' + g.r.IntN(9)))
		b.WriteString(g.digits(pickInt(g, wholeLengths, wholeExtremes, 1) - 1))
		if g.chance(20) {
			b.WriteString(strings.Repeat("0", g.r.IntN(6)))
		}
	}
	if g.chance(50) {
		b.WriteByte('.')
		switch {
		case g.chance(15):
			b.WriteString(strings.Repeat("0", 1+g.r.IntN(5)))
		default:
			b.WriteString(g.digits(pickInt(g, decimalLengths, decimalExtremes, 1)))
			if g.chance(30) {
				b.WriteString(strings.Repeat("0", 1+g.r.IntN(5)))
			}
		}
	}
	if g.chance(40) {
		b.WriteString([]string{"e", "E", "e+", "E+", "e-", "E-"}[g.r.IntN(6)])
		if g.chance(10) {
			b.WriteString(strings.Repeat("0", 1+g.r.IntN(3)))
		}
		b.WriteString(strconv.Itoa(pickInt(g, exponents, exponentExtrema, 8)))
	}
	return b.String()
}

var (
	documentKeys    = []string{"a", "b", "c", "amount", "id", "é", "", " ", "a b", "A", "aa", "ab", "b", "😀", "key\"quote", "k\\", "\u007f", "<&>"}
	documentStrings = []string{"", "a", "é", "é", "😀", "a\tb\nc", `quote"`, `back\slash`, "\u007f", "￿", "<&>", "/", " ",
		strings.Repeat("long", 60), "1", "true", "null"}
)

func jsonText(text string) string {
	raw, _ := json.Marshal(text)
	return string(raw)
}

// escaped writes a string as JSON, now and then with every character as a
// \u escape, which is the same string written another way.
func (g *numbers) escaped(text string) string {
	if !g.chance(15) {
		return jsonText(text)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		if r > 0xffff {
			r -= 0x10000
			b.WriteString(`\u` + strconv.FormatInt(int64(0xd800+(r>>10)), 16) + `\u` + strconv.FormatInt(int64(0xdc00+(r&0x3ff)), 16))
			continue
		}
		b.WriteString(`\u` + strings.Repeat("0", 4-len(strconv.FormatInt(int64(r), 16))) + strconv.FormatInt(int64(r), 16))
	}
	b.WriteByte('"')
	return b.String()
}

// fitting writes a number Postgres can store, of modest length.
func (g *numbers) fitting() string {
	for {
		n := g.number()
		if len(n) < 200 && events.NumberFits(n) {
			return n
		}
	}
}

func (g *numbers) space() string {
	return []string{"", "", "", " ", "\n", "\t ", "  "}[g.r.IntN(7)]
}

// document writes a JSON value: mostly an object, with keys in any order
// and sometimes twice.
func (g *numbers) document(depth int) string {
	switch k := g.r.IntN(20); {
	case k < 9 && depth > 0:
		fields := make([]string, g.r.IntN(6))
		for i := range fields {
			fields[i] = g.space() + g.escaped(documentKeys[g.r.IntN(len(documentKeys))]) + g.space() + ":" + g.space() + g.document(depth-1)
		}
		return "{" + strings.Join(fields, ",") + g.space() + "}"
	case k < 11 && depth > 0:
		items := make([]string, g.r.IntN(4))
		for i := range items {
			items[i] = g.space() + g.document(depth-1)
		}
		return "[" + strings.Join(items, ",") + g.space() + "]"
	case k < 16:
		return g.fitting()
	case k < 18:
		return g.escaped(documentStrings[g.r.IntN(len(documentStrings))])
	case k < 19:
		return []string{"true", "false"}[g.r.IntN(2)]
	}
	return "null"
}
