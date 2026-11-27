package mandates

import (
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Two fuzz targets that need no database. A normal test run plays their
// seeds; a hunt mutates them, guided by coverage:
//
//	go test -run '^$' -fuzz FuzzCompareDecimals -fuzztime 2m ./modules/mandates/
//	go test -run '^$' -fuzz FuzzCondition -fuzztime 2m ./modules/mandates/
//
// What a hunt finds, Go keeps under testdata/fuzz, and it runs with the
// seeds from then on. The comparison with the database is in
// evaluators_random_test.go.

var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// asRational reads a JSON number exactly, with math/big, for numbers small
// enough that this is cheap.
func asRational(n string) (*big.Rat, bool) {
	if len(n) > 300 || !jsonNumber.MatchString(n) {
		return nil, false
	}
	if i := strings.IndexAny(n, "eE"); i >= 0 {
		if exponent, err := strconv.Atoi(n[i+1:]); err != nil || exponent > 2000 || exponent < -2000 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(n)
}

// compareDecimals orders two JSON numbers as arithmetic does, however they
// are spelled, and the other way round when they are swapped.
func FuzzCompareDecimals(f *testing.F) {
	for _, pair := range [][2]string{
		{"500", "500.0"}, {"5e2", "500"}, {"0.5e3", "500"}, {"50000e-2", "500"}, {"0", "-0"}, {"0.000", "0e10"},
		{"499.99", "500"}, {"500.000000000000000000001", "500"}, {"-1", "0"}, {"-0.1", "-0.01"}, {"1e-300", "0"},
		{"1E+21", "1000000000000000000000"}, {"9223372036854775807", "9223372036854775808"}, {"0.1", "0.10000000000000001"},
		{"-1e300", "1e-300"}, {"12.34e1", "123.4"}, {"1e2000", "1e1999"}, {"-0.0", "0e-5"},
	} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		got, ok := compareDecimals(a, b)
		x, xok := asRational(a)
		y, yok := asRational(b)
		if !xok || !yok {
			// Not a number this test can check by other means. It must
			// still be answered, without a panic.
			return
		}
		if !ok {
			t.Fatalf("compareDecimals(%q, %q) cannot compare two JSON numbers", a, b)
		}
		if want := x.Cmp(y); got != want {
			t.Fatalf("compareDecimals(%q, %q) = %d, and arithmetic says %d", a, b, got, want)
		}
		if back, _ := compareDecimals(b, a); back != -got {
			t.Fatalf("compareDecimals(%q, %q) = %d, but swapped it is %d", a, b, got, back)
		}
	})
}

// Whatever a person types as a condition, the compiler answers, with a
// plan or a refusal. A plan it gives reads back as itself, and evaluating
// it on any payload answers too: holding only when true, and saying what
// stands in the way when it does not hold.
func FuzzCondition(f *testing.F) {
	for _, seed := range [][3]string{
		{`payload.amount <= 500`, "a.b", `{"amount": 500.0}`},
		{`payload.amount <= 500 && payload.currency in ["EUR", "SEK"]`, "refunds.issue", `{"amount": 1e2, "currency": "SEK"}`},
		{`500 >= payload.amount`, "a.b", `{"amount": "400"}`},
		{`payload["a-b"] == 1 || !(has(payload.note))`, "a.b", `{"a-b": 1, "note": null}`},
		{`has(payload.a.b)`, "a.b", `{"a": ["b"]}`},
		{`action in ["a.b", "a.c"] && payload.vip != false`, "a.c", `{"vip": true}`},
		{`payload.n < 18446744073709551615u`, "a.b", `{"n": 18446744073709551614}`},
		{`payload.name < "z"`, "a.b", `{"name": "é"}`},
		{`payload.x == -0.5`, "", `[1]`},
		{`payload.name.startsWith("x")`, "a.b", `{}`},
		{`payload.amount <=`, "a.b", `not json`},
		{`payload[""] == 1`, "a.b", `{"": 1}`},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, source, action, payload string) {
		plan, err := CompileCondition(source)
		if err != nil {
			return
		}
		raw, err := json.Marshal(plan)
		if err != nil {
			t.Fatalf("a compiled plan does not write as JSON: %v\n%s", err, source)
		}
		var read Plan
		if err := json.Unmarshal(raw, &read); err != nil {
			t.Fatalf("a compiled plan does not read back: %v\n%s", err, raw)
		}
		again, err := json.Marshal(read)
		if err != nil || string(again) != string(raw) {
			t.Fatalf("a compiled plan reads back as another plan\nwritten: %s\nread:    %s", raw, again)
		}
		value, known := plan.Evaluate(action, json.RawMessage(payload))
		holds := plan.Holds(action, json.RawMessage(payload))
		if holds != (known && value) {
			t.Fatalf("a condition holds only when it is true: holds %v, value %v, known %v\n%s on %s", holds, value, known, source, payload)
		}
		unmet := plan.Unmet(action, json.RawMessage(payload))
		if holds && len(unmet) > 0 {
			t.Fatalf("a condition that holds names something in its way: %q\n%s on %s", unmet, source, payload)
		}
		if !holds && len(unmet) == 0 {
			t.Fatalf("a condition that does not hold names nothing in its way\n%s on %s", source, payload)
		}
	})
}
