package mandates

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates/internal/repo"
)

func TestCompareDecimals(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"500", "500", 0},
		{"500", "500.0", 0},
		{"5e2", "500", 0},
		{"0.5e3", "500", 0},
		{"50000e-2", "500", 0},
		{"0", "-0", 0},
		{"0.000", "0e10", 0},
		{"499.99", "500", -1},
		{"500.000000000000000000001", "500", 1},
		{"-1", "0", -1},
		{"-2", "-1", -1},
		{"-0.1", "-0.01", -1},
		{"0.1", "0.10000000000000001", -1},
		{"123456789012345678901234567890", "123456789012345678901234567891", -1},
		{"1e100000", "9e99999", 1},
		{"1e-100000", "0", 1},
		{"-1e-100000", "0", -1},
		{"10", "9.99999", 1},
		{"1E+2", "100", 0},
	} {
		got, ok := compareDecimals(c.a, c.b)
		require.True(t, ok, c.a+" "+c.b)
		require.Equal(t, c.want, got, c.a+" vs "+c.b)
		back, ok := compareDecimals(c.b, c.a)
		require.True(t, ok)
		require.Equal(t, -c.want, back, c.b+" vs "+c.a)
	}
	for _, bad := range []string{"", "-", "1.", ".5", "1e", "1e99999999999", "0x10", "1_000", "+1", "NaN"} {
		_, ok := compareDecimals(bad, "1")
		require.False(t, ok, bad)
	}
}

func TestCompileCondition(t *testing.T) {
	for source, want := range map[string]string{
		`payload.amount <= 500 && payload.currency in ["EUR", "SEK"]`: `{"op":"and","args":[{"op":"<=","path":["payload","amount"],"value":500},{"op":"in","path":["payload","currency"],"values":["EUR","SEK"]}]}`,
		`500 >= payload.amount`:                    `{"op":"<=","path":["payload","amount"],"value":500}`,
		`-0.5 < payload.x`:                         `{"op":">","path":["payload","x"],"value":-0.5}`,
		`payload.x != -3`:                          `{"op":"!=","path":["payload","x"],"value":-3}`,
		`payload.x == 0.1`:                         `{"op":"==","path":["payload","x"],"value":0.1}`,
		`payload.x < 18446744073709551615u`:        `{"op":"<","path":["payload","x"],"value":18446744073709551615}`,
		`!(payload.a == true) || has(payload.b.c)`: `{"op":"or","args":[{"op":"not","args":[{"op":"==","path":["payload","a"],"value":true}]},{"op":"has","path":["payload","b","c"]}]}`,
		`payload["a-b"].c == "x"`:                  `{"op":"==","path":["payload","a-b","c"],"value":"x"}`,
		`action == "refunds.issue"`:                `{"op":"==","path":["action"],"value":"refunds.issue"}`,
		`action in ["a.b", "a.c"]`:                 `{"op":"in","path":["action"],"values":["a.b","a.c"]}`,
		"  payload.note == \"<b>\\\"q\\\"</b>\"  ": `{"op":"==","path":["payload","note"],"value":"<b>\"q\"</b>"}`,
	} {
		plan, err := CompileCondition(source)
		require.NoError(t, err, source)
		got, err := json.Marshal(plan)
		require.NoError(t, err)
		var a, b any
		require.NoError(t, json.Unmarshal(got, &a))
		require.NoError(t, json.Unmarshal([]byte(want), &b))
		require.Equal(t, b, a, source)
	}
	for source, why := range map[string]string{
		``:                                   "empty",
		`   `:                                "empty",
		`payload.amount <=`:                  "not valid CEL",
		`amount <= 5`:                        "not valid CEL",
		`payload.amount`:                     "must be true or false",
		`payload.amount + 1 <= 5`:            "comparison that is not between a field and a literal",
		`payload.a == payload.b`:             "comparing two fields",
		`5 == 5`:                             "comparison that is not between a field and a literal",
		`payload.name.startsWith("x")`:       "the function startsWith",
		`size(payload.items) < 3`:            "not between a field and a literal",
		`payload.x == null`:                  "comparing with null",
		`payload.x == b"ab"`:                 "bytes",
		`payload.x in []`:                    "1 to 100 values",
		`payload.x in [1, "a"]`:              "mixes types",
		`payload.x in payload.y`:             "list of literal values",
		`payload.flag > true`:                "ordering true and false",
		`action < "x"`:                       "ordering action keys",
		`has(payload)`:                       "not valid CEL",
		`payload.items.exists(i, i > 2)`:     "not a comparison",
		`payload.amount <= 1e400`:            "not valid CEL",
		`payload.code == "a\x00"`:            "NUL",
		`payload["a\u0000b"] == 1`:           "NUL",
		"payload.code == \"a\x00\"":          "NUL",
		`payload[payload.k] == 1`:            "index that is not a literal string",
		`payload.a.b.c.d.e.f.g.h.i.j.k == 1`: "levels deep",
		`payload == {"a": 1}`:                "the payload as a whole",
		strings.Repeat("payload.a==1||", 110) + "payload.a==1": "more than 100 parts",
		strings.Repeat(" ", 2001):                              "at most 2000 characters",
	} {
		_, err := CompileCondition(source)
		require.Error(t, err, source)
		require.Contains(t, err.Error(), why, source)
		// The web app shows a refusal at the condition field when it names the condition.
		require.Contains(t, strings.ToLower(err.Error()), "condition", source)
	}
}

func TestCheckAgainstSchemas(t *testing.T) {
	schemas := []json.RawMessage{
		json.RawMessage(`{"type":"object","properties":{"amount":{"type":"integer"},"currency":{"type":"string"},"customer":{"type":"object","properties":{"vip":{"type":"boolean"}}},"free":{}}}`),
		json.RawMessage(`{"type":"object","properties":{"amount":{"type":["number","null"]},"ref":{"$ref":"#/defs/x"},"any":{"anyOf":[{"type":"string"}]}}}`),
	}
	patterns := []string{"refunds.*"}
	check := func(source string) error {
		plan, err := CompileCondition(source)
		require.NoError(t, err, source)
		return plan.CheckAgainstSchemas(patterns, schemas)
	}
	for _, ok := range []string{
		`payload.amount <= 500`,
		`payload.currency in ["EUR"]`,
		`payload.customer.vip == true`,
		`has(payload.customer.vip)`,
		`payload.free == "anything"`,
		`payload.ref.deep == 1`,
		`payload.any.deep == 1`,
		`action == "refunds.issue"`,
		`action in ["refunds.issue", "refunds.void"]`,
	} {
		require.NoError(t, check(ok), ok)
	}
	for source, why := range map[string]string{
		`payload.amout <= 500`:                     "names payload.amout, which no action",
		`payload.amount <= "500"`:                  "compares payload.amount with a string",
		`payload.currency in [1, 2]`:               "compares payload.currency with a number",
		`payload.customer.vip == "yes"`:            "with a string",
		`payload.customer.name == "x"`:             "names payload.customer.name, which no action",
		`has(payload.nope)`:                        "names payload.nope, which no action",
		`action == "tickets.close"`:                `the action "tickets.close"`,
		`action in ["refunds.issue", "x"]`:         `the action "x"`,
		`payload.amount <= 5 || payload.nope == 1`: "names payload.nope, which no action",
	} {
		err := check(source)
		require.Error(t, err, source)
		require.Contains(t, err.Error(), why, source)
		require.Contains(t, err.Error(), "condition", source)
	}
	plan, err := CompileCondition(`payload.whatever == 1`)
	require.NoError(t, err)
	require.NoError(t, plan.CheckAgainstSchemas(patterns, nil), "without schemas the payload cannot be checked")
	require.NoError(t, plan.CheckAgainstSchemas(patterns, []json.RawMessage{json.RawMessage(`{"type":"object"}`)}), "a schema without properties names none")
	require.Error(t, plan.CheckAgainstSchemas(patterns, []json.RawMessage{json.RawMessage(`{"type":"object","properties":{}}`)}), "an empty list of properties names none either")
	require.NoError(t, plan.CheckAgainstSchemas(patterns, append(schemas, nil)), "an action without a schema may send any field")
	for schema, open := range map[string]bool{
		`{"properties":{"a":{}},"additionalProperties":{"type":"number"}}`: true,
		`{"properties":{"a":{}},"additionalProperties":true}`:              true,
		`{"properties":{"a":{}},"unevaluatedProperties":{}}`:               true,
		`{"properties":{"a":{}},"additionalProperties":false}`:             false,
		`{"properties":{"a":{}},"unevaluatedProperties":false}`:            false,
	} {
		err := plan.CheckAgainstSchemas(patterns, []json.RawMessage{json.RawMessage(schema)})
		require.Equal(t, open, err == nil, schema)
	}

	// Only an action the condition requires must be covered.
	for source, ok := range map[string]bool{
		`action != "billing.refund" && payload.amount <= 5`:      true,
		`!(action in ["billing.refund"]) && payload.amount <= 5`: true,
		`!(action == "billing.refund")`:                          true,
		`!(action != "billing.refund")`:                          false,
		`!!(action == "billing.refund")`:                         false,
		`action == "billing.refund" || payload.amount <= 5`:      false,
	} {
		plan, err := CompileCondition(source)
		require.NoError(t, err, source)
		require.Equal(t, ok, plan.CheckAgainstSchemas(patterns, schemas) == nil, source)
	}
}

// A stored plan that reads only in part is not used in part: the mandate
// then holds for nothing, as in the database, which refuses such a plan.
func TestAPlanThatDoesNotReadNeverHolds(t *testing.T) {
	condition := `payload.a == 1 || payload.b == 2`
	for _, raw := range []string{
		`{"op":"or","args":[{"op":"==","path":["payload","a"],"value":1},{"op":5}]}`,
		`{"op":"or","args":[{"op":"==","path":["payload","a"],"value":1}],"path":7}`,
		`not json`,
	} {
		m := toMandate(repo.MandateMandate{Condition: &condition, ConditionPlan: []byte(raw)})
		require.Equal(t, condition, m.Condition)
		require.False(t, m.ConditionHolds("a.b", json.RawMessage(`{"a": 1}`)), raw)
	}
	m := toMandate(repo.MandateMandate{})
	require.True(t, m.ConditionHolds("a.b", json.RawMessage(`{}`)), "no condition, nothing to hold")
}

func TestUnmetNamesWhatStandsInTheWay(t *testing.T) {
	for _, c := range []struct {
		condition, payload string
		want               []string
	}{
		{`payload.amount <= 500 && payload.currency in ["EUR", "SEK"]`, `{"amount": 750, "currency": "SEK"}`,
			[]string{`payload.amount <= 500 (payload.amount is 750)`}},
		{`payload.amount <= 500 && payload.currency in ["EUR", "SEK"]`, `{"currency": "USD"}`,
			[]string{`payload.amount <= 500 (payload.amount is missing)`, `payload.currency in ["EUR", "SEK"] (payload.currency is "USD")`}},
		{`payload.amount <= 500 && payload.currency in ["EUR", "SEK"]`, `{"amount": 5, "currency": "EUR"}`, nil},
		{`payload.a == 1 || payload["b-c"] == 2`, `{"a": 2, "b-c": "2"}`,
			[]string{`payload.a == 1 (payload.a is 2)`, `payload["b-c"] == 2 (payload["b-c"] is "2")`}},
		{`!(payload.vip == true && payload.amount > 100)`, `{"vip": true, "amount": 200}`,
			[]string{`!(payload.vip == true) (payload.vip is true)`, `!(payload.amount > 100) (payload.amount is 200)`}},
		{`has(payload.order.id)`, `{"order": 5}`, []string{`has(payload.order.id) (payload.order is not an object)`}},
		{`has(payload.note)`, `{}`, []string{`has(payload.note)`}},
		{`action == "refunds.issue"`, `{}`, []string{`action == "refunds.issue" (action is "tickets.close")`}},
		{`payload.text == "x"`, `{"text": "` + strings.Repeat("y", 200) + `"}`,
			[]string{`payload.text == "x" (payload.text is "` + strings.Repeat("y", 96) + `...)`}},
		{`payload.text == "x"`, `{"text": "` + strings.Repeat("å", 200) + `"}`,
			[]string{`payload.text == "x" (payload.text is "` + strings.Repeat("å", 96) + `...)`}},
	} {
		plan, err := CompileCondition(c.condition)
		require.NoError(t, err, c.condition)
		got := plan.Unmet("tickets.close", json.RawMessage(c.payload))
		require.Equal(t, c.want, got, c.condition+" on "+c.payload)
		for _, line := range got {
			require.True(t, utf8.ValidString(line), "a line goes into a protobuf string, which must be valid UTF-8")
		}
	}
}
