package mandates_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/testkit"
)

// A condition is evaluated twice: by the service (mandates.Plan) and by the
// database (mandate.condition_holds). conditions_test.go holds the two
// together with cases written by hand. This suite does it with cases
// nobody thought of: it generates conditions, plans and payloads from a
// seed, runs every one through both, and fails on any difference.
//
// It runs a modest number of cases by default, from a fixed seed, so a
// normal test run is the same every time. For a longer hunt:
//
//	MANDATE_RANDOM_CASES=2000000 MANDATE_RANDOM_SEED=random go test -timeout 30m \
//	    -run TestEvaluatorsSuite ./modules/mandates/
//
// A failure prints the seed and the case. Add the case to the table in
// conditions_test.go with its fix, so it stays fixed.
type EvaluatorsSuite struct {
	IntegrationSuite
}

func TestEvaluatorsSuite(t *testing.T) {
	suite.Run(t, new(EvaluatorsSuite))
}

const (
	defaultRandomCases = 20_000
	randomBatch        = 500
	// A run shorter than this is too short to ask that every answer has
	// its share of the cases.
	minCasesForShares = 2_000
)

// evaluatorCase is one plan on one action and payload.
type evaluatorCase struct {
	// source is the condition the plan was compiled from, or empty for a
	// plan written directly.
	source  string
	plan    string
	action  string
	payload string
}

func (c evaluatorCase) String() string {
	return fmt.Sprintf("condition: %s\nplan:      %s\naction:    %q\npayload:   %s", c.source, c.plan, c.action, c.payload)
}

// sqlAnswer is what the database says of one case.
type sqlAnswer struct {
	holds *bool
	// valid is the database's check of the plan. It must answer yes or no:
	// a CHECK constraint passes on NULL, so NULL would let a plan in.
	valid *bool
	// stored is the plan as the database keeps it and hands it back, which
	// is how the service reads a plan: a key written twice is there once.
	stored string
}

func (a sqlAnswer) String() string {
	if a.holds == nil {
		return "unknown"
	}
	return strconv.FormatBool(*a.holds)
}

// sqlEvaluate runs a batch through mandate.condition_holds and
// mandate.valid_condition_plan, in one statement.
func (s *EvaluatorsSuite) sqlEvaluate(ctx context.Context, batch []evaluatorCase) ([]sqlAnswer, error) {
	plans, actions, payloads := make([]string, len(batch)), make([]string, len(batch)), make([]string, len(batch))
	for i, c := range batch {
		plans[i], actions[i], payloads[i] = c.plan, c.action, c.payload
	}
	answers := make([]sqlAnswer, 0, len(batch))
	err := db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT mandate.condition_holds(c.plan::jsonb, c.action, c.payload::jsonb), mandate.valid_condition_plan(c.plan::jsonb),
				(c.plan::jsonb)::text
			FROM unnest($1::text[], $2::text[], $3::text[]) WITH ORDINALITY AS c(plan, action, payload, i)
			ORDER BY c.i`, plans, actions, payloads)
		if err != nil {
			return err
		}
		answers, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (sqlAnswer, error) {
			var a sqlAnswer
			return a, row.Scan(&a.holds, &a.valid, &a.stored)
		})
		return err
	})
	return answers, err
}

// A condition the compiler accepts gives a plan the database takes, and the
// service and the database give the same answer for it on any payload,
// unknown included.
func (s *EvaluatorsSuite) Test_CompiledConditionsAgree() {
	cases, seed := testkit.RandomRun(s.T(), defaultRandomCases)
	g := newGenerator(seed)
	tenant := s.W.NewTenant(s.T(), "Acme")
	var (
		batch                   []evaluatorCase
		plans                   []mandates.Plan
		compiled, refused       int
		truths, falses, unknown int
	)
	flush := func() {
		answers, err := s.sqlEvaluate(tenant.Owner, batch)
		s.Require().NoError(err, "the database could not read a batch the generator promised it could; seed %d", seed)
		for i, c := range batch {
			want := goHolds(plans[i], c.action, c.payload)
			s.Require().Equal(want, answers[i].String(), "the service says %s and the database says %s (seed %d)\n%s", want, answers[i], seed, c)
			s.Require().NotNil(answers[i].valid, "the database's check of a plan answers NULL, which a CHECK lets through (seed %d)\n%s", seed, c)
			s.Require().True(*answers[i].valid, "the database refuses a plan the compiler wrote (seed %d)\n%s", seed, c)
			s.Require().Equal(want == "true", plans[i].Holds(c.action, json.RawMessage(c.payload)), "only true holds (seed %d)\n%s", seed, c)
			switch want {
			case "true":
				truths++
			case "false":
				falses++
			default:
				unknown++
			}
		}
		batch, plans = batch[:0], plans[:0]
	}
	for compiled < cases {
		// The payload first, so that the condition can aim at it.
		payload := g.payload()
		source := g.condition(3)
		plan, err := mandates.CompileCondition(source)
		if err != nil {
			refused++
			s.Require().Less(refused, 10*cases+1000, "the generator writes almost nothing the compiler takes; last: %s: %v", source, err)
			continue
		}
		compiled++
		raw, err := json.Marshal(plan)
		s.Require().NoError(err)
		batch = append(batch, evaluatorCase{source: source, plan: string(raw), action: g.action(), payload: payload})
		plans = append(plans, plan)
		if len(batch) == randomBatch {
			flush()
		}
	}
	flush()
	s.T().Logf("%d conditions compiled, %d refused by the compiler; answers: %d true, %d false, %d unknown", compiled, refused, truths, falses, unknown)
	// A run that never reaches one of the answers has tested little.
	if cases >= minCasesForShares {
		for name, n := range map[string]int{"true": truths, "false": falses, "unknown": unknown} {
			s.Require().Greater(n, cases/50, "too few cases answer %s", name)
		}
	}
}

// A plan nobody compiled, such as one written straight into the database,
// meets the database's own check first. The check answers yes or no, never
// NULL. What it accepts, the service can read, and the two answer alike.
func (s *EvaluatorsSuite) Test_APlanTheDatabaseAcceptsIsReadAlike() {
	cases, seed := testkit.RandomRun(s.T(), defaultRandomCases)
	g := newGenerator(seed)
	tenant := s.W.NewTenant(s.T(), "Acme")
	var accepted, refused, done int
	for done < cases {
		batch := make([]evaluatorCase, 0, randomBatch)
		for len(batch) < randomBatch && done+len(batch) < cases {
			payload := g.payload()
			batch = append(batch, evaluatorCase{plan: g.rawPlan(3), action: g.action(), payload: payload})
		}
		answers, err := s.sqlEvaluate(tenant.Owner, batch)
		s.Require().NoError(err, "the database could not read a batch the generator promised it could; seed %d", seed)
		for i, c := range batch {
			s.Require().NotNil(answers[i].valid, "the database's check of a plan answers NULL, which a CHECK lets through (seed %d)\n%s", seed, c)
			var plan mandates.Plan
			readErr := json.Unmarshal([]byte(answers[i].stored), &plan)
			if !*answers[i].valid {
				refused++
				// Never stored, so nothing to agree on. The service must
				// still not fall over on one it can read.
				if readErr == nil {
					goHolds(plan, c.action, c.payload)
				}
				continue
			}
			accepted++
			s.Require().NoError(readErr, "the database's check accepts a plan the service cannot read (seed %d)\n%s", seed, c)
			want := goHolds(plan, c.action, c.payload)
			s.Require().Equal(want, answers[i].String(), "the service says %s and the database says %s (seed %d)\n%s", want, answers[i], seed, c)
			// What the check accepts is what the compiler writes: read and
			// written again by the service, it is the same plan.
			again, err := json.Marshal(plan)
			s.Require().NoError(err)
			s.Require().Equal(canonical(s.T(), answers[i].stored), canonical(s.T(), string(again)), "the service reads a plan the database accepts as another plan (seed %d)\n%s", seed, c)
		}
		done += len(batch)
	}
	s.T().Logf("%d plans: the database's check accepts %d and refuses %d", done, accepted, refused)
	if cases >= minCasesForShares {
		s.Require().Greater(accepted, cases/20, "too few plans the database's check accepts")
		s.Require().Greater(refused, cases/20, "too few plans the database's check refuses")
	}
}

// canonical is a JSON text in the form the log uses, in which two texts of
// the same value are the same text, whatever their numbers' spelling.
func canonical(t *testing.T, text string) string {
	t.Helper()
	out, err := events.Canonical(json.RawMessage(text))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, text)
	}
	return string(out)
}

// generator writes conditions, plans and payloads from a seed. Its values
// come from small pools, so that a condition and a payload often name the
// same field and hold values that are equal, or nearly.
type generator struct {
	r *rand.Rand
	// known are the values of the payload written last, so that a
	// condition can name a field that is there and compare it with a value
	// of its kind.
	known []knownValue
}

// knownValue is one value in a payload: where it is, its kind (0 a number,
// 1 a string, 2 a boolean, 3 anything else) and its JSON text.
type knownValue struct {
	path []string
	kind int
	text string
}

func newGenerator(seed uint64) *generator {
	return &generator{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // a test's generator
}

func pick[T any](g *generator, from []T) T {
	return from[g.r.IntN(len(from))]
}

func (g *generator) chance(percent int) bool {
	return g.r.IntN(100) < percent
}

var (
	fieldKeys = []string{"a", "b", "c", "amount", "n", "name", "vip", "note", "customer", "country",
		"0", "a-b", "é", "A", " ", "with space", "x.y", "payload", "action", "has"}
	stringPool = []string{"", "a", "b", "A", "Z", "ab", "a ", "z", "é", "aé", "é", "SE", "NO", "DK", "true", "1", "500",
		"refunds.issue", "tickets.close", "a.b", "😀", "a\tb", `quote"`, `back\slash`, "\u007f", "￿", "x/y", "<&>",
		strings.Repeat("long", 80)}
	actionPool = []string{"a.b", "a.c", "refunds.issue", "tickets.close", "", "A.B", "é", "a.b "}
	numberPool = []string{"0", "-0", "0.0", "-0.0", "0e5", "1", "-1", "2", "10", "100", "500", "500.0", "5e2", "0.5e3", "50000e-2",
		"499.99", "500.000000000000000000000001", "0.1", "0.10", "1e-1", "0.10000000000000001", "-0.5", "1e21", "1E21", "1e+21",
		"123456789012345678901234567890", "-1e300", "1e300", "1e-300", "9223372036854775807", "9223372036854775808",
		"18446744073709551615", "18446744073709551614", "1e-7", "5e-7", "0.0000005", "1.50e1", "15.0", "15", "3", "3.0", "2.999999999999999999"}
	compareOps = []string{"==", "!=", "<", "<=", ">", ">="}
)

// number writes a JSON number Postgres can store: from the pool, a second
// spelling of a pool number, or a new one.
func (g *generator) number() string {
	for {
		var n string
		switch g.r.IntN(4) {
		case 0, 1:
			n = pick(g, numberPool)
		case 2:
			n = g.respell(pick(g, numberPool))
		default:
			n = g.freshNumber()
		}
		if events.NumberFits(n) {
			return n
		}
	}
}

// respell writes the same number another way: zeros after the point, or
// the point moved and an exponent to make up for it.
func (g *generator) respell(n string) string {
	mantissa, exponent := n, 0
	if i := strings.IndexAny(n, "eE"); i >= 0 {
		e, err := strconv.Atoi(n[i+1:])
		if err != nil {
			return n
		}
		mantissa, exponent = n[:i], e
	}
	sign := ""
	if rest, negative := strings.CutPrefix(mantissa, "-"); negative {
		sign, mantissa = "-", rest
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	switch g.r.IntN(3) {
	case 0:
		fraction += strings.Repeat("0", 1+g.r.IntN(4))
	case 1:
		// 12.5 is 1250e-2.
		zeros := g.r.IntN(4)
		whole, exponent = whole+fraction+strings.Repeat("0", zeros), exponent-len(fraction)-zeros
		fraction = ""
		if trimmed := strings.TrimLeft(whole, "0"); trimmed != "" {
			whole = trimmed
		} else {
			whole = "0"
		}
	default:
		// 12.5 is 0.0125e3.
		shift := 1 + g.r.IntN(3)
		whole, fraction, exponent = "0", strings.Repeat("0", shift-1)+whole+fraction, exponent+len(whole)+shift-1
	}
	out := sign + whole
	if fraction != "" {
		out += "." + fraction
	}
	if exponent != 0 || g.chance(20) {
		out += pick(g, []string{"e", "E"})
		if exponent >= 0 && g.chance(50) {
			out += "+"
		}
		out += strconv.Itoa(exponent)
	}
	return out
}

func (g *generator) digits(n int) string {
	var b strings.Builder
	for range n {
		b.WriteByte(byte('0' + g.r.IntN(10)))
	}
	return b.String()
}

func (g *generator) freshNumber() string {
	var b strings.Builder
	if g.chance(30) {
		b.WriteByte('-')
	}
	if g.chance(20) {
		b.WriteByte('0')
	} else {
		b.WriteByte(byte('1' + g.r.IntN(9)))
		b.WriteString(g.digits(g.r.IntN(25)))
	}
	if g.chance(40) {
		b.WriteByte('.')
		b.WriteString(g.digits(1 + g.r.IntN(25)))
	}
	if g.chance(30) {
		b.WriteString(pick(g, []string{"e", "E", "e+", "E+", "e-", "E-"}))
		b.WriteString(strconv.Itoa(pick(g, []int{0, 1, 2, 5, 10, 20, 30, 100, 300, 5000})))
	}
	return b.String()
}

func jsonString(text string) string {
	raw, _ := json.Marshal(text)
	return string(raw)
}

// value writes a JSON value of any kind, and remembers it under its path.
func (g *generator) value(depth int, path []string) string {
	kind, text := 3, ""
	switch k := g.r.IntN(20); {
	case k < 8:
		kind, text = 0, g.number()
	case k < 13:
		kind, text = 1, jsonString(pick(g, stringPool))
	case k < 15:
		kind, text = 2, pick(g, []string{"true", "false"})
	case k < 16:
		text = "null"
	case k < 18 && depth > 0:
		return g.object(depth-1, path)
	case depth > 0:
		items := make([]string, g.r.IntN(3))
		for i := range items {
			items[i] = g.value(depth-1, nil)
		}
		text = "[" + strings.Join(items, ",") + "]"
	default:
		kind, text = 0, g.number()
	}
	if path != nil {
		g.known = append(g.known, knownValue{path: path, kind: kind, text: text})
	}
	return text
}

// object writes a JSON object. Now and then it writes a key twice: both
// sides must then read the last one, so only the last one is remembered.
func (g *generator) object(depth int, path []string) string {
	fields := make([]string, g.r.IntN(6))
	for i := range fields {
		key := pick(g, fieldKeys)
		var under []string
		if path != nil {
			under = append(append([]string{}, path...), key)
			g.forget(under)
		}
		fields[i] = jsonString(key) + pick(g, []string{":", ": ", " : "}) + g.value(depth, under)
	}
	return "{" + strings.Join(fields, pick(g, []string{",", ", "})) + "}"
}

// forget drops what was remembered at a path or below it: the key is being
// written again.
func (g *generator) forget(path []string) {
	kept := g.known[:0]
	for _, v := range g.known {
		if len(v.path) < len(path) || strings.Join(v.path[:len(path)], "\x00") != strings.Join(path, "\x00") {
			kept = append(kept, v)
		}
	}
	g.known = kept
}

// payload is almost always an object, as a proposal's is.
func (g *generator) payload() string {
	g.known = g.known[:0]
	if g.chance(3) {
		return g.value(1, nil)
	}
	return g.object(2, []string{})
}

func (g *generator) action() string {
	return pick(g, actionPool)
}

// celString writes a string as a CEL literal.
func celString(text string) string {
	return strconv.Quote(text)
}

var celIdentifier = map[string]bool{"a": true, "b": true, "c": true, "amount": true, "n": true, "name": true, "vip": true,
	"note": true, "customer": true, "country": true, "A": true, "payload": true, "action": true}

// steps names a field of the payload: usually one the payload written last
// has, with the value found there.
func (g *generator) steps() ([]string, *knownValue) {
	if len(g.known) > 0 && g.chance(75) {
		v := pick(g, g.known)
		return v.path, &v
	}
	path := make([]string, 1+g.r.IntN(3))
	for i := range path {
		path[i] = pick(g, fieldKeys)
		if g.chance(2) {
			path[i] = ""
		}
	}
	return path, nil
}

// celPath writes a path as a condition names it: payload.a.b, or
// payload["a-b"] where a name is not an identifier.
func (g *generator) celPath(path []string) string {
	var b strings.Builder
	b.WriteString("payload")
	for _, key := range path {
		if celIdentifier[key] && g.chance(85) {
			b.WriteString("." + key)
		} else {
			b.WriteString("[" + celString(key) + "]")
		}
	}
	return b.String()
}

// kindFor is the kind of literal to compare a field with: mostly the kind
// of the value that is there.
func (g *generator) kindFor(v *knownValue) int {
	if v != nil && v.kind < 3 && g.chance(85) {
		return v.kind
	}
	return pick(g, []int{0, 0, 0, 1, 1, 2})
}

// literalNear writes a CEL literal of a kind: now and then the very value
// the payload holds, so that equality and the edges of an order are met.
func (g *generator) literalNear(kind int, v *knownValue) string {
	if v != nil && v.kind == kind && g.chance(35) {
		switch kind {
		case 0:
			// A JSON number is not always a CEL literal; the compiler
			// refuses what is not, and the test skips it.
			if strings.ContainsAny(v.text, ".eE") || g.chance(50) {
				return v.text
			}
			return v.text + ".0"
		case 1:
			var text string
			if json.Unmarshal([]byte(v.text), &text) == nil {
				return celString(text)
			}
		default:
			return v.text
		}
	}
	return g.celLiteral(kind)
}

// celNumber writes a number as a CEL literal: an int, a uint or a double.
func (g *generator) celNumber() string {
	switch g.r.IntN(6) {
	case 0:
		return strconv.FormatUint(g.r.Uint64(), 10) + "u"
	case 1:
		return strconv.FormatInt(g.r.Int64()-g.r.Int64(), 10)
	case 2:
		return strconv.FormatFloat(g.r.NormFloat64()*1000, 'g', -1, 64)
	}
	n := g.number()
	// A CEL double needs a point or an exponent, and a digit before the point.
	if g.chance(15) && !strings.ContainsAny(n, ".eE") {
		n += ".0"
	}
	return n
}

func (g *generator) celLiteral(kind int) string {
	switch kind {
	case 0:
		return g.celNumber()
	case 1:
		return celString(pick(g, stringPool))
	}
	return pick(g, []string{"true", "false"})
}

// condition writes a condition in CEL. Most of what it writes is inside the
// subset the compiler takes; the compiler refuses the rest, and the test
// skips it.
func (g *generator) condition(depth int) string {
	if depth > 0 {
		switch k := g.r.IntN(10); {
		case k < 2:
			return "(" + g.condition(depth-1) + " && " + g.condition(depth-1) + ")"
		case k < 4:
			return "(" + g.condition(depth-1) + " || " + g.condition(depth-1) + ")"
		case k < 5:
			return "!(" + g.condition(depth-1) + ")"
		}
	}
	switch k := g.r.IntN(20); {
	case k < 10:
		path, known := g.steps()
		kind := g.kindFor(known)
		op := pick(g, compareOps)
		if g.chance(15) {
			// The literal on the left.
			return g.literalNear(kind, known) + " " + op + " " + g.celPath(path)
		}
		return g.celPath(path) + " " + op + " " + g.literalNear(kind, known)
	case k < 14:
		path, known := g.steps()
		kind := g.kindFor(known)
		values := make([]string, 1+g.r.IntN(4))
		for i := range values {
			values[i] = g.literalNear(kind, known)
			if g.chance(3) {
				values[i] = g.celLiteral(g.r.IntN(3))
			}
		}
		return g.celPath(path) + " in [" + strings.Join(values, ", ") + "]"
	case k < 17:
		path, _ := g.steps()
		if g.chance(30) {
			path = append(append([]string{}, path...), pick(g, fieldKeys))
		}
		return "has(" + g.celPath(path) + ")"
	case k < 19:
		return "action " + pick(g, []string{"==", "!="}) + " " + celString(pick(g, actionPool))
	}
	values := make([]string, 1+g.r.IntN(3))
	for i := range values {
		values[i] = celString(pick(g, actionPool))
	}
	return "action in [" + strings.Join(values, ", ") + "]"
}

// rawPath writes a plan's path, sometimes one no compiler would.
func (g *generator) rawPath() string {
	switch k := g.r.IntN(40); {
	case k == 0:
		return pick(g, []string{`null`, `"payload"`, `[]`, `{}`, `["payload"]`, `["tenant","a"]`, `["action","a"]`, `[1]`})
	case k == 1:
		return `["payload",` + pick(g, []string{`1`, `null`, `true`, `["a"]`, `""`}) + `]`
	case k < 6:
		return `["action"]`
	}
	path, _ := g.steps()
	steps := []string{`"payload"`}
	for _, key := range path {
		steps = append(steps, jsonString(key))
	}
	return "[" + strings.Join(steps, ",") + "]"
}

// scalar writes a literal a plan may hold: a number, a string or a boolean.
func (g *generator) scalar(kind int) string {
	switch kind {
	case 0:
		return g.number()
	case 1:
		return jsonString(pick(g, append(stringPool, actionPool...)))
	}
	return pick(g, []string{"true", "false"})
}

// rawPlan writes a plan as JSON directly: mostly well formed, sometimes
// not, so that both sides also meet what the compiler never writes.
func (g *generator) rawPlan(depth int) string {
	fields := map[string]string{}
	op := pick(g, compareOps)
	switch k := g.r.IntN(20); {
	case k < 5 && depth > 0:
		op = pick(g, []string{"and", "or", "not"})
		n := 1 + g.r.IntN(3)
		if op == "not" && g.chance(90) {
			n = 1
		}
		if g.chance(3) {
			n = 0
		}
		args := make([]string, n)
		for i := range args {
			args[i] = g.rawPlan(depth - 1)
			if g.chance(1) {
				args[i] = pick(g, []string{`1`, `null`, `"and"`, `[]`})
			}
		}
		fields["args"] = "[" + strings.Join(args, ",") + "]"
	case k < 8:
		op = "in"
		kind := g.r.IntN(3)
		values := make([]string, g.r.IntN(5))
		for i := range values {
			values[i] = g.scalar(kind)
			if g.chance(3) {
				values[i] = g.value(1, nil)
			}
		}
		fields["path"], fields["values"] = g.rawPath(), "["+strings.Join(values, ",")+"]"
	case k < 10:
		op = "has"
		fields["path"] = g.rawPath()
	default:
		fields["path"], fields["value"] = g.rawPath(), g.scalar(g.r.IntN(3))
		if g.chance(4) {
			fields["value"] = g.value(1, nil)
		}
	}
	if g.chance(2) {
		op = pick(g, []string{"xor", "", "AND", "=", "=="})
	}
	fields["op"] = jsonString(op)
	// What is wrong with a plan more often than its values: a field that is
	// missing, of another type, written twice, or not a plan's field at all.
	if g.chance(3) {
		delete(fields, pick(g, []string{"op", "path", "value", "values", "args"}))
	}
	if g.chance(3) {
		fields[pick(g, []string{"op", "path", "value", "values", "args"})] = pick(g, []string{`null`, `1`, `"x"`, `[]`, `{}`, `true`})
	}
	if g.chance(3) {
		fields[pick(g, []string{"OP", "Op", "Path", "VALUE", "Values", "ARGS", "extra"})] = pick(g, []string{jsonString(pick(g, compareOps)), g.rawPath(), g.scalar(g.r.IntN(3)), `[]`})
	}
	parts := make([]string, 0, len(fields)+1)
	for _, name := range []string{"op", "OP", "Op", "path", "Path", "value", "VALUE", "values", "Values", "args", "ARGS", "extra"} {
		if v, ok := fields[name]; ok {
			parts = append(parts, jsonString(name)+":"+v)
		}
	}
	if g.chance(2) && len(parts) > 0 {
		// The same field twice: the last one counts.
		parts = append(parts, `"op":`+jsonString(pick(g, compareOps)))
	}
	if g.chance(1) {
		return pick(g, []string{`null`, `[]`, `"=="`, `1`, `{}`})
	}
	return "{" + strings.Join(parts, ",") + "}"
}
