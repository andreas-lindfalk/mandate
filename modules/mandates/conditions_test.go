package mandates_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type ConditionsSuite struct {
	IntegrationSuite
}

func TestConditionsSuite(t *testing.T) {
	suite.Run(t, new(ConditionsSuite))
}

// sqlHolds is mandate.condition_holds: "true", "false" or "unknown" for NULL.
func (s *ConditionsSuite) sqlHolds(ctx context.Context, plan json.RawMessage, action string, payload string) string {
	s.T().Helper()
	var result *bool
	s.Require().NoError(db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mandate.condition_holds($1::jsonb, $2, $3::jsonb)`, string(plan), action, payload).Scan(&result)
	}))
	if result == nil {
		return "unknown"
	}
	return fmt.Sprint(*result)
}

func goHolds(plan mandates.Plan, action, payload string) string {
	value, known := plan.Evaluate(action, json.RawMessage(payload))
	if !known {
		return "unknown"
	}
	return fmt.Sprint(value)
}

// The service and the database evaluate the same plan. Every case runs
// through both, and both must give the same answer, unknown included.
func (s *ConditionsSuite) Test_GoAndPostgresAgree() {
	t := s.W.NewTenant(s.T(), "Acme")
	cases := []struct{ condition, action, payload, want string }{
		{`payload.amount <= 500`, "a.b", `{"amount": 500}`, "true"},
		{`payload.amount <= 500`, "a.b", `{"amount": 500.0}`, "true"},
		{`payload.amount <= 500`, "a.b", `{"amount": 5e2}`, "true"},
		{`payload.amount <= 500`, "a.b", `{"amount": 500.000000000000000000000001}`, "false"},
		{`payload.amount <= 500`, "a.b", `{"amount": -1e300}`, "true"},
		{`payload.amount <= 500`, "a.b", `{"amount": "400"}`, "unknown"},
		{`payload.amount <= 500`, "a.b", `{}`, "unknown"},
		{`payload.amount <= 500`, "a.b", `{"amount": null}`, "unknown"},
		{`payload.amount <= 500`, "a.b", `{"amount": [1]}`, "unknown"},
		{`payload.amount == 0.1`, "a.b", `{"amount": 0.1}`, "true"},
		{`payload.amount == 0.1`, "a.b", `{"amount": 0.10000000000000001}`, "false"},
		{`payload.amount > -0.5`, "a.b", `{"amount": 0}`, "true"},
		{`payload.amount == 0`, "a.b", `{"amount": -0}`, "true"},
		{`payload.n < 18446744073709551615u`, "a.b", `{"n": 18446744073709551614}`, "true"},
		{`payload.n < 9223372036854775807`, "a.b", `{"n": 123456789012345678901234567890}`, "false"},
		{`payload.name < "b"`, "a.b", `{"name": "a"}`, "true"},
		{`payload.name < "a"`, "a.b", `{"name": "Z"}`, "true"},
		{`payload.name < "z"`, "a.b", `{"name": "é"}`, "false"},
		{`payload.name == "aé"`, "a.b", `{"name": "aé"}`, "true"},
		{`payload.name == "x"`, "a.b", `{"name": "X"}`, "false"},
		{`payload.name != "x"`, "a.b", `{"name": 5}`, "unknown"},
		{`payload.vip == true`, "a.b", `{"vip": true}`, "true"},
		{`payload.vip == true`, "a.b", `{"vip": "true"}`, "unknown"},
		{`payload.vip != false`, "a.b", `{"vip": false}`, "false"},
		{`payload.customer.country in ["SE", "NO"]`, "a.b", `{"customer": {"country": "NO"}}`, "true"},
		{`payload.customer.country in ["SE", "NO"]`, "a.b", `{"customer": {"country": "DK"}}`, "false"},
		{`payload.customer.country in ["SE", "NO"]`, "a.b", `{"customer": "SE"}`, "unknown"},
		{`payload.customer.country in ["SE", "NO"]`, "a.b", `{"customer": {"country": 1}}`, "unknown"},
		{`payload.n in [1, 2.0, 3e0]`, "a.b", `{"n": 2}`, "true"},
		{`payload["0"] == "x"`, "a.b", `{"0": "x"}`, "true"},
		{`payload.list["0"] == "x"`, "a.b", `{"list": ["x"]}`, "unknown"},
		{`payload["a-b"] == 1`, "a.b", `{"a-b": 1}`, "true"},
		{`has(payload.note)`, "a.b", `{"note": null}`, "true"},
		{`has(payload.note)`, "a.b", `{}`, "false"},
		{`has(payload.a.b)`, "a.b", `{"a": {}}`, "false"},
		{`has(payload.a.b)`, "a.b", `{"a": ["b"]}`, "unknown"},
		{`has(payload.a.b)`, "a.b", `{}`, "unknown"},
		{`!(payload.amount > 500)`, "a.b", `{"amount": 400}`, "true"},
		{`!(payload.amount > 500)`, "a.b", `{}`, "unknown"},
		{`payload.a == 1 && payload.b == 2`, "a.b", `{"a": 1}`, "unknown"},
		{`payload.a == 1 && payload.b == 2`, "a.b", `{"a": 2}`, "false"},
		{`payload.a == 1 || payload.b == 2`, "a.b", `{"a": 1}`, "true"},
		{`payload.a == 1 || payload.b == 2`, "a.b", `{"a": 2}`, "unknown"},
		{`payload.a == 1 || payload.b == 2`, "a.b", `{"a": 2, "b": 3}`, "false"},
		{`action == "refunds.issue" && payload.amount <= 500 || action == "tickets.close"`, "tickets.close", `{}`, "true"},
		{`action == "refunds.issue" && payload.amount <= 500 || action == "tickets.close"`, "refunds.issue", `{"amount": 600}`, "false"},
		{`action == "refunds.issue" && payload.amount <= 500 || action == "tickets.close"`, "refunds.issue", `{}`, "unknown"},
		{`action in ["a.b", "a.c"]`, "a.c", `{}`, "true"},
		{`action != "a.b"`, "a.b", `{}`, "false"},
	}
	for _, c := range cases {
		plan, err := mandates.CompileCondition(c.condition)
		s.Require().NoError(err, c.condition)
		raw, err := json.Marshal(plan)
		s.Require().NoError(err)
		name := c.condition + " on " + c.action + " " + c.payload
		s.Require().Equal(c.want, goHolds(plan, c.action, c.payload), "Go: "+name)
		s.Require().Equal(c.want, s.sqlHolds(t.Owner, raw, c.action, c.payload), "Postgres: "+name)
		s.Require().Equal(c.want == "true", plan.Holds(c.action, json.RawMessage(c.payload)), "only true holds: "+name)
		s.Require().True(s.validPlan(t.Owner, raw), "the database takes every plan the compiler writes: "+c.condition)
	}

	// A plan the compiler would never write still gets the same answer
	// from both, and never true.
	for _, raw := range []string{
		`{}`, `{"op":"and"}`, `{"op":"and","args":[]}`, `{"op":"or","args":[]}`, `{"op":"not","args":[]}`,
		`{"op":"not","args":[{"op":"==","path":["payload","a"],"value":2},{"op":"==","path":["payload","a"],"value":1}]}`,
		`{"op":"xor","args":[{"op":"==","path":["payload","a"],"value":1}]}`,
		`{"op":"==","path":["payload","a"]}`, `{"op":"==","path":"a","value":1}`, `{"op":"==","path":[],"value":1}`,
		`{"op":"==","path":["payload"],"value":{"a":1}}`, `{"op":"==","path":["tenant"],"value":1}`, `{"op":"==","path":["payload",1],"value":1}`,
		`{"op":"==","path":["action","x"],"value":"a.b"}`, `{"op":"<","path":["payload","b"],"value":true}`,
		`{"op":"==","path":["payload","o"],"value":{"x":1}}`, `{"op":"==","path":["payload","n"],"value":null}`,
		`{"op":"in","path":["payload","a"],"values":[]}`, `{"op":"in","path":["payload","a"],"values":[1,"1"]}`, `{"op":"in","path":["payload","a"]}`,
		`{"op":"has","path":["payload"]}`, `{"op":"has","path":["action","a"]}`, `{"op":"has","path":["payload",1]}`,
		`{"op":"not","args":[{"op":"in","path":["payload","n"],"values":[null]}]}`,
		`{"op":"not","args":[{"op":"in","path":["payload","o"],"values":[{"x":1}]}]}`,
		`{"op":"not","args":[{"op":"==","path":["payload","o"],"value":{"x":1}}]}`,
	} {
		payload := `{"a": 1, "b": true, "o": {"x": 1}, "n": null}`
		var plan mandates.Plan
		got := "unknown"
		if json.Unmarshal([]byte(raw), &plan) == nil {
			got = goHolds(plan, "a.b", payload)
		}
		s.Require().Equal(got, s.sqlHolds(t.Owner, json.RawMessage(raw), "a.b", payload), raw)
		s.Require().NotEqual("true", got, raw)
	}
}

func (s *ConditionsSuite) Test_TheGrantChecksTheCondition() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "refunds.issue", Risk: mandates.RiskLow, MayExecute: true, Connector: actions.ConnectorWebhook, Url: "https://connector.example/hooks",
		Schema: json.RawMessage(`{"type":"object","properties":{"amount":{"type":"number"},"currency":{"type":"string"}}}`),
	})
	s.Require().NoError(err)
	grant := func(actions []string, condition string) (mandates.Mandate, error) {
		return s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: actions, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30, Condition: condition})
	}
	for condition, why := range map[string]string{
		`payload.amout <= 500`:                "names payload.amout, which no action",
		`payload.amount <= "500"`:             "with a string",
		`action == "tickets.close"`:           "does not cover",
		`payload.amount <=`:                   "not valid CEL",
		`payload.name.startsWith("x")`:        "startsWith",
		`payload.amount <= 500 && amount > 1`: "not valid CEL",
	} {
		_, err := grant([]string{"refunds.*"}, condition)
		s.code(err, fault.InvalidArgument)
		s.Require().Contains(err.Error(), why, condition)
	}

	m, err := grant([]string{"refunds.*"}, "  payload.amount <= 500 && payload.currency in [\"EUR\", \"SEK\"]\n")
	s.Require().NoError(err)
	s.Require().Equal(`payload.amount <= 500 && payload.currency in ["EUR", "SEK"]`, m.Condition, "stored trimmed")
	read, err := s.W.Kernel.Mandates.Get(t.Owner, m.Id)
	s.Require().NoError(err)
	s.Require().Equal(m.Condition, read.Condition)
	s.Require().True(read.ConditionHolds("refunds.issue", json.RawMessage(`{"amount": 499, "currency": "SEK"}`)))
	s.Require().False(read.ConditionHolds("refunds.issue", json.RawMessage(`{"amount": 501, "currency": "SEK"}`)))
	var granted events.Stored
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		var ok bool
		granted, ok, err = events.Last(t.Owner, tx, m.Id, "mandate.granted")
		s.Require().True(ok)
		return err
	}))
	var payload map[string]any
	s.Require().NoError(json.Unmarshal(granted.Payload, &payload))
	s.Require().Equal(m.Condition, payload["condition"], "the log records the condition granted")

	// Without a schema to check against, a builtin action's fields are the
	// host's to know, also when it is covered beside an action with one.
	_, err = grant([]string{"test.low"}, `payload.anything == 1`)
	s.Require().NoError(err)
	_, err = grant([]string{"refunds.*", "test.*"}, `payload.anything == 1`)
	s.Require().NoError(err)
	_, err = s.W.Kernel.Actions.Define(t.Owner, actions.Input{Key: "refunds.note", Risk: mandates.RiskLow, Connector: actions.ConnectorBuiltin})
	s.Require().NoError(err)
	_, err = grant([]string{"refunds.*"}, `payload.anything == 1`)
	s.Require().NoError(err, "a defined action without a schema takes any field")
	s.Require().NoError(s.W.Kernel.Actions.Remove(t.Owner, "refunds.note"))
	_, err = grant([]string{"refunds.*"}, `payload.anything == 1`)
	s.code(err, fault.InvalidArgument)
	plain, err := grant([]string{"refunds.*"}, "")
	s.Require().NoError(err)
	s.Require().Empty(plain.Condition)
	s.Require().True(plain.ConditionHolds("refunds.issue", json.RawMessage(`{}`)), "no condition narrows nothing")
}

// The table holds the condition as granted: never changed later, never a
// text without its plan or a plan without its text.
func (s *ConditionsSuite) Test_TheDatabaseKeepsTheConditionAsGranted() {
	t := s.W.NewTenant(s.T(), "Acme")
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"test.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30, Condition: `payload.n <= 1`})
	s.Require().NoError(err)
	s.refused(t.Owner, `UPDATE mandate.mandates SET condition_plan = '{"op":"<=","path":["payload","n"],"value":1000}' WHERE id = $1`, m.Id)
	s.refused(t.Owner, `UPDATE mandate.mandates SET condition = NULL, condition_plan = NULL WHERE id = $1`, m.Id)
	s.refused(t.Owner, `UPDATE mandate.mandates SET revoked_at = now(), condition = 'payload.n <= 1000' WHERE id = $1`, m.Id)

	insert := `INSERT INTO mandate.mandates (id, tenant_id, actions, max_risk, budget_count, policy_ref, valid_until, condition, condition_plan)
		VALUES (gen_random_uuid(), $1, '{test.*}', 'low', 10, 'P', now() + interval '1 day', $2, $3::jsonb)`
	s.invalid(t.Owner, insert, t.Id, "payload.n <= 1", nil)
	s.invalid(t.Owner, insert, t.Id, nil, `{"op":"<=","path":["payload","n"],"value":1}`)
	s.invalid(t.Owner, insert, t.Id, "payload.n <= 1", `[]`)
	s.invalid(t.Owner, insert, t.Id, "", `{"op":"<=","path":["payload","n"],"value":1}`)

	// A plan the compiler would never write is refused, however it got there.
	deep := `{"op":"==","path":["payload","n"],"value":1}`
	for range 70 {
		deep = `{"op":"not","args":[` + deep + `]}`
	}
	for name, plan := range map[string]string{
		"unknown op":         `{"op":"xor","args":[{"op":"==","path":["payload","n"],"value":1}]}`,
		"extra key":          `{"op":"==","path":["payload","n"],"value":1,"sql":"x"}`,
		"null literal":       `{"op":"==","path":["payload","n"],"value":null}`,
		"object literal":     `{"op":"==","path":["payload","n"],"value":{"a":1}}`,
		"ordering booleans":  `{"op":"<","path":["payload","b"],"value":true}`,
		"ordering actions":   `{"op":"<","path":["action"],"value":"a.b"}`,
		"number for action":  `{"op":"==","path":["action"],"value":5}`,
		"payload as a whole": `{"op":"==","path":["payload"],"value":1}`,
		"another root":       `{"op":"==","path":["tenant","id"],"value":1}`,
		"mixed in":           `{"op":"in","path":["payload","n"],"values":[1,"1"]}`,
		"empty in":           `{"op":"in","path":["payload","n"],"values":[]}`,
		"empty and":          `{"op":"and","args":[]}`,
		"not of two":         `{"op":"not","args":[{"op":"has","path":["payload","a"]},{"op":"has","path":["payload","b"]}]}`,
		"has on action":      `{"op":"has","path":["action"]}`,
		// Found by random plans (evaluators_random_test.go): the check
		// answered NULL for these, and a CHECK passes on NULL; inside an
		// and, or or not, the node was skipped and the plan passed.
		"no path":                 `{"op":"==","value":1}`,
		"no value":                `{"op":"==","path":["payload","n"]}`,
		"no fields":               `{"op":"=="}`,
		"no path in has":          `{"op":"has"}`,
		"no values":               `{"op":"in","path":["payload","n"]}`,
		"no op":                   `{"path":["payload","n"],"value":1}`,
		"an op that is no string": `{"op":1,"path":["payload","n"],"value":1}`,
		"a part with no fields":   `{"op":"and","args":[{"op":"=="}]}`,
		"a part with no path":     `{"op":"or","args":[{"op":"has","path":["payload","a"]},{"op":"==","value":1}]}`,
		"a not of no fields":      `{"op":"not","args":[{"op":"in"}]}`,
		// A field of another op, which the service cannot read.
		"args on a comparison": `{"op":"==","path":["payload","n"],"value":1,"args":"x"}`,
		"args on has":          `{"op":"has","path":["payload","n"],"args":[]}`,
		"a value on has":       `{"op":"has","path":["payload","n"],"value":1}`,
		"values on ==":         `{"op":"==","path":["payload","n"],"value":1,"values":[1]}`,
		"a path on and":        `{"op":"and","args":[{"op":"has","path":["payload","a"]}],"path":["payload","a"]}`,
		"too deep":             deep,
		"too big":              `{"op":"in","path":["payload","n"],"values":["` + strings.Repeat("x", 20_000) + `"]}`,
	} {
		s.Require().False(s.validPlan(t.Owner, json.RawMessage(plan)), name)
		s.invalid(t.Owner, insert, t.Id, "payload.n == 1", plan)
	}
}

func (s *ConditionsSuite) validPlan(ctx context.Context, plan json.RawMessage) bool {
	s.T().Helper()
	var valid bool
	s.Require().NoError(db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mandate.valid_condition_plan($1::jsonb)`, string(plan)).Scan(&valid)
	}))
	return valid
}

// Unmet explains with the mandates Resolve would use: live by the
// database's clock, not yet valid ones left out.
func (s *ConditionsSuite) Test_UnmetReadsTheMandatesResolveReads() {
	t := s.W.NewTenant(s.T(), "Acme")
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.low"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30, Condition: `payload.n <= 1`})
	s.Require().NoError(err)
	lines, err := s.W.Kernel.Mandates.Unmet(t.Owner, t.AgentId, "test.low", mandates.RiskLow, json.RawMessage(`{"n": 2}`))
	s.Require().NoError(err)
	s.Require().Equal([]string{"mandate " + m.Id.String()[:8] + ": payload.n <= 1 (payload.n is 2)"}, lines)
	// Not yet valid, by the database's clock: the window moved an hour on.
	_, err = s.W.Pg.Owner.Exec(context.Background(), `BEGIN; SET LOCAL session_replication_role = replica;
		UPDATE mandate.mandates SET valid_from = valid_from + interval '1 hour', valid_until = valid_until + interval '1 hour' WHERE id = '`+m.Id.String()+`'; COMMIT;`)
	s.Require().NoError(err)
	lines, err = s.W.Kernel.Mandates.Unmet(t.Owner, t.AgentId, "test.low", mandates.RiskLow, json.RawMessage(`{"n": 2}`))
	s.Require().NoError(err)
	s.Require().Empty(lines, "a mandate not yet valid explains nothing")
	other, err := s.W.Kernel.Mandates.Unmet(t.Owner, uuid.New(), "test.low", mandates.RiskLow, json.RawMessage(`{"n": 2}`))
	s.Require().NoError(err)
	s.Require().Empty(other, "another agent's mandate explains nothing")
}

func (s *ConditionsSuite) Test_ResolveFollowsTheCondition() {
	t := s.W.NewTenant(s.T(), "Acme")
	small, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.low"}, MaxRisk: mandates.RiskMedium, BudgetCount: 10, PolicyRef: "Small", Days: 30, Condition: `payload.amount <= 500`})
	s.Require().NoError(err)
	resolve := func(risk, payload string) mandates.Resolution {
		var r mandates.Resolution
		s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
			var err error
			r, err = s.W.Kernel.Mandates.Resolve(t.System, tx, t.AgentId, "test.low", risk, json.RawMessage(payload))
			return err
		}))
		return r
	}
	r := resolve(mandates.RiskLow, `{"amount": 400}`)
	s.Require().Empty(r.Reason)
	s.Require().Equal(small.Id, r.Mandate.Id)
	s.Require().Equal(mandates.SkipConditionUnmet, resolve(mandates.RiskLow, `{"amount": 600}`).Reason)
	s.Require().Equal(mandates.SkipConditionUnmet, resolve(mandates.RiskLow, `{"amount": "400"}`).Reason, "a wrong type does not hold")
	s.Require().Equal(mandates.SkipConditionUnmet, resolve(mandates.RiskLow, `{}`).Reason, "a missing field does not hold")
	s.Require().Equal(mandates.SkipRiskAbove, resolve(mandates.RiskHigh, `{"amount": 400}`).Reason, "the risk is told before the condition")

	// A second mandate without a condition, at a lower risk: the first is
	// more permissive and chosen when it holds; the second when it does not.
	unconditional, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "Any", Days: 30})
	s.Require().NoError(err)
	s.Require().Equal(small.Id, resolve(mandates.RiskLow, `{"amount": 400}`).Mandate.Id)
	s.Require().Equal(unconditional.Id, resolve(mandates.RiskLow, `{"amount": 600}`).Mandate.Id)
	s.Require().Equal(mandates.SkipConditionUnmet, resolve(mandates.RiskMedium, `{"amount": 600}`).Reason,
		"at medium risk only the conditional mandate allows the risk, and its condition is false")
}
