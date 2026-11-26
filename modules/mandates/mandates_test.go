package mandates_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type MandatesSuite struct {
	IntegrationSuite
}

func TestMandatesSuite(t *testing.T) {
	suite.Run(t, new(MandatesSuite))
}

func (s *MandatesSuite) grant(ctx context.Context, input mandates.Input) mandates.Mandate {
	s.T().Helper()
	if input.PolicyRef == "" {
		input.PolicyRef = "POLICY-1"
	}
	if input.Days == 0 {
		input.Days = 30
	}
	if input.BudgetCount == 0 {
		input.BudgetCount = 10
	}
	if input.MaxRisk == "" {
		input.MaxRisk = mandates.RiskLow
	}
	m, err := s.W.Kernel.Mandates.Grant(ctx, input)
	s.Require().NoError(err)
	return m
}

func (s *MandatesSuite) resolve(ctx context.Context, agentId uuid.UUID, action, risk string) mandates.Resolution {
	s.T().Helper()
	var r mandates.Resolution
	s.Require().NoError(db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		var err error
		r, err = s.W.Kernel.Mandates.Resolve(ctx, tx, agentId, action, risk, json.RawMessage(`{}`))
		return err
	}))
	return r
}

func (s *MandatesSuite) Test_GrantValidation() {
	t := s.W.NewTenant(s.T(), "Acme")
	ok := mandates.Input{Actions: []string{"tickets.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30}
	for name, change := range map[string]func(*mandates.Input){
		"no actions":      func(i *mandates.Input) { i.Actions = nil },
		"bad pattern":     func(i *mandates.Input) { i.Actions = []string{"Tickets.*"} },
		"high risk":       func(i *mandates.Input) { i.MaxRisk = mandates.RiskHigh },
		"unknown risk":    func(i *mandates.Input) { i.MaxRisk = "none" },
		"zero budget":     func(i *mandates.Input) { i.BudgetCount = 0 },
		"negative budget": func(i *mandates.Input) { i.BudgetCount = -1 },
		"no policy":       func(i *mandates.Input) { i.PolicyRef = "  " },
		"too long":        func(i *mandates.Input) { i.Days = 367 },
		"zero days":       func(i *mandates.Input) { i.Days = 0 },
	} {
		input := ok
		change(&input)
		_, err := s.W.Kernel.Mandates.Grant(t.Owner, input)
		s.code(err, fault.InvalidArgument)
		s.T().Log(name, "refused")
	}
	_, err := s.W.Kernel.Mandates.Grant(context.Background(), ok)
	s.code(err, fault.InvalidArgument)
}

// A policy reference that is too long is refused with the rule, not with
// the message for a missing one, and 200 characters are still allowed.
func (s *MandatesSuite) Test_GrantPolicyRefLengthStatesTheLimit() {
	t := s.W.NewTenant(s.T(), "Acme")
	input := mandates.Input{Actions: []string{"tickets.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, Days: 30}
	for name, ref := range map[string]string{"missing": "  ", "too long": strings.Repeat("ä", 201)} {
		input.PolicyRef = ref
		_, err := s.W.Kernel.Mandates.Grant(t.Owner, input)
		s.code(err, fault.InvalidArgument)
		s.Require().Equal("a mandate rests on a policy document: give its reference, 1 to 200 characters", fault.Message(err), name)
	}
	input.PolicyRef = strings.Repeat("ä", 200)
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, input)
	s.Require().NoError(err)
	s.Require().Equal(input.PolicyRef, m.PolicyRef)
}

func (s *MandatesSuite) Test_OwnerGrantsAndRevokesOnce() {
	t := s.W.NewTenant(s.T(), "Acme")
	m := s.grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"tickets.*", "refunds.issue"}, MaxRisk: mandates.RiskMedium, BudgetCount: 42, PolicyRef: "ACME-AI-POLICY §4", Days: 90})
	s.Require().Equal(&t.OwnerId, m.GrantedBy)
	s.Require().Equal(&t.AgentId, m.AgentId)
	s.Require().Equal(int64(42), m.BudgetCount)
	s.Require().WithinDuration(time.Now().AddDate(0, 0, 90), m.ValidUntil, time.Minute)
	s.Require().Equal([]string{"mandate.granted"}, s.eventTypes(t.Owner, m.Id))

	live, err := s.W.Kernel.Mandates.List(t.Reviewer, false)
	s.Require().NoError(err)
	s.Require().Len(live, 1)
	spent, err := s.W.Kernel.Mandates.Spent(t.Reviewer, m.Id)
	s.Require().NoError(err)
	s.Require().Zero(spent)

	s.Require().NoError(s.W.Kernel.Mandates.Revoke(t.Owner, m.Id))
	s.code(s.W.Kernel.Mandates.Revoke(t.Owner, m.Id), fault.Conflict)
	s.code(s.W.Kernel.Mandates.Revoke(t.Reviewer, m.Id), fault.PermissionDenied)
	live, err = s.W.Kernel.Mandates.List(t.Owner, false)
	s.Require().NoError(err)
	s.Require().Empty(live)
	all, err := s.W.Kernel.Mandates.List(t.Owner, true)
	s.Require().NoError(err)
	s.Require().Len(all, 1)
	s.Require().NotNil(all[0].RevokedAt)
	s.Require().Equal(&t.OwnerId, all[0].RevokedBy)
	s.Require().Equal([]string{"mandate.granted", "mandate.revoked"}, s.eventTypes(t.Owner, m.Id))
}

func (s *MandatesSuite) Test_OnlyOwners() {
	t := s.W.NewTenant(s.T(), "Acme")
	input := mandates.Input{Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 1, PolicyRef: "P", Days: 1}
	_, err := s.W.Kernel.Mandates.Grant(t.Reviewer, input)
	s.code(err, fault.PermissionDenied)
	_, err = s.W.Kernel.Mandates.Grant(t.Agent, input)
	s.code(err, fault.PermissionDenied)
	_, err = s.W.Kernel.Mandates.Grant(t.System, input)
	s.code(err, fault.PermissionDenied)
	m := s.grant(t.Owner, input)
	s.code(s.W.Kernel.Mandates.Revoke(t.Reviewer, m.Id), fault.PermissionDenied)
	s.code(s.W.Kernel.Mandates.Revoke(t.Agent, m.Id), fault.PermissionDenied)
}

func (s *MandatesSuite) Test_DatabaseRefusesWhatGoRefuses() {
	t := s.W.NewTenant(s.T(), "Acme")
	insert := `INSERT INTO mandate.mandates (id, tenant_id, agent_id, actions, max_risk, budget_count, policy_ref, valid_until)
		VALUES (gen_random_uuid(), $1, NULL, $2, $3, $4, $5, now() + ($6 || ' days')::interval)`
	s.invalid(t.Owner, insert, t.Id, []string{"*"}, "high", 10, "P", "30")
	s.invalid(t.Owner, insert, t.Id, []string{"Tickets.create"}, "low", 10, "P", "30")
	s.invalid(t.Owner, insert, t.Id, []string{}, "low", 10, "P", "30")
	s.invalid(t.Owner, insert, t.Id, []string{"*"}, "low", 0, "P", "30")
	s.invalid(t.Owner, insert, t.Id, []string{"*"}, "low", 10, "", "30")
	s.invalid(t.Owner, insert, t.Id, []string{"*"}, "low", 10, "P", "400")
	s.refused(t.Reviewer, insert, t.Id, []string{"*"}, "low", 10, "P", "30")
	s.refused(t.Agent, insert, t.Id, []string{"*"}, "low", 10, "P", "30")

	m := s.grant(t.Owner, mandates.Input{Actions: []string{"tickets.*"}})
	s.refused(t.Owner, `UPDATE mandate.mandates SET actions = '{*}' WHERE id = $1`, m.Id)
	s.refused(t.Owner, `UPDATE mandate.mandates SET max_risk = 'medium' WHERE id = $1`, m.Id)
	s.refused(t.Owner, `UPDATE mandate.mandates SET valid_until = now() + interval '10 years' WHERE id = $1`, m.Id)
	s.refused(t.Owner, `UPDATE mandate.mandates SET budget_count = 1000000 WHERE id = $1`, m.Id)
	s.refused(t.Owner, `DELETE FROM mandate.mandates WHERE id = $1`, m.Id)
	s.refused(t.Reviewer, `UPDATE mandate.mandates SET revoked_at = now() WHERE id = $1`, m.Id)
	s.Require().NoError(s.exec(t.Owner, `UPDATE mandate.mandates SET revoked_at = now() WHERE id = $1`, m.Id))
	s.refused(t.Owner, `UPDATE mandate.mandates SET revoked_at = NULL WHERE id = $1`, m.Id)
	s.refused(t.Owner, `UPDATE mandate.mandates SET revoked_at = now() WHERE id = $1`, m.Id)
	got, err := s.W.Kernel.Mandates.Get(t.Owner, m.Id)
	s.Require().NoError(err)
	s.Require().Equal(&t.OwnerId, got.RevokedBy, "the trigger records who revoked, whatever the statement said")
}

func (s *MandatesSuite) Test_ResolveFindsTheMandateThatAllowsIt() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow).Reason)

	other, err := s.W.Kernel.Agents.Create(t.Owner, "other-bot", "", "live")
	s.Require().NoError(err)
	s.grant(t.Owner, mandates.Input{AgentId: &other.Id, Actions: []string{"*"}, MaxRisk: mandates.RiskMedium})
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow).Reason, "another agent's mandate does not cover me")

	low := s.grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 3})
	r := s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow)
	s.Require().Empty(r.Reason)
	s.Require().Equal(low.Id, r.Mandate.Id)
	s.Require().Zero(r.Spent)
	s.Require().Equal(mandates.SkipRiskAbove, s.resolve(t.System, t.AgentId, "test.medium", mandates.RiskMedium).Reason)
	s.Require().Equal(mandates.SkipRiskAbove, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskHigh).Reason)
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "tickets.create", mandates.RiskLow).Reason)

	medium := s.grant(t.Owner, mandates.Input{Actions: []string{"test.low"}, MaxRisk: mandates.RiskMedium})
	r = s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow)
	s.Require().Equal(medium.Id, r.Mandate.Id, "the most permissive mandate with room is chosen")
}

// resolveWithin resolves in a transaction of its own that waits at most a
// second for a lock, so a resolution that queues behind another fails with
// lock_not_available instead of waiting.
func (s *MandatesSuite) resolveWithin(ctx context.Context, agentId uuid.UUID, action string) (mandates.Resolution, error) {
	var r mandates.Resolution
	err := db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '1s'"); err != nil {
			return err
		}
		var err error
		r, err = s.W.Kernel.Mandates.Resolve(ctx, tx, agentId, action, mandates.RiskLow, json.RawMessage(`{}`))
		return err
	})
	return r, err
}

func (s *MandatesSuite) Test_ResolveLocksOnlyTheMandatesThatCoverTheAction() {
	t := s.W.NewTenant(s.T(), "Acme")
	other, err := s.W.Kernel.Agents.Create(t.Owner, "other-bot", "", "live")
	s.Require().NoError(err)
	own := s.grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"refunds.*"}})
	tenantWide := s.grant(t.Owner, mandates.Input{Actions: []string{"tickets.*"}})

	// The first agent resolves refunds.create and holds its transaction
	// open; the tenant-wide mandate for tickets is live for it too, but does
	// not cover the action.
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		r, err := s.W.Kernel.Mandates.Resolve(t.System, tx, t.AgentId, "refunds.create", mandates.RiskLow, json.RawMessage(`{}`))
		s.Require().NoError(err)
		s.Require().Equal(own.Id, r.Mandate.Id)

		r, err = s.resolveWithin(t.System, other.Id, "tickets.create")
		s.Require().NoError(err, "another agent's execution of another action does not wait")
		s.Require().Equal(tenantWide.Id, r.Mandate.Id)
		return nil
	}))

	// A mandate that covers the action is still locked: a second execution
	// under it waits for the first, so the budget is counted in turn.
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		r, err := s.W.Kernel.Mandates.Resolve(t.System, tx, t.AgentId, "tickets.create", mandates.RiskLow, json.RawMessage(`{}`))
		s.Require().NoError(err)
		s.Require().Equal(tenantWide.Id, r.Mandate.Id)

		_, err = s.resolveWithin(t.System, other.Id, "tickets.create")
		var pgErr *pgconn.PgError
		s.Require().ErrorAs(err, &pgErr)
		s.Require().Equal("55P03", pgErr.Code, "lock_not_available")
		return nil
	}))
}

func (s *MandatesSuite) Test_ValidityAndRevocation() {
	t := s.W.NewTenant(s.T(), "Acme")
	m := s.grant(t.Owner, mandates.Input{Actions: []string{"*"}, Days: 10})
	s.Require().Empty(s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow).Reason)

	// Eleven days on, by the database's clock: the mandate's window moved
	// back, past the guard.
	s.shift(m.Id, "-11 days")
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow).Reason, "expired")
	live, err := s.W.Kernel.Mandates.List(t.Owner, false)
	s.Require().NoError(err)
	s.Require().Empty(live, "the list reads the same clock")
	s.shift(m.Id, "11 days")

	s.Require().NoError(s.W.Kernel.Mandates.Revoke(t.Owner, m.Id))
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow).Reason, "revoked")
}

func (s *MandatesSuite) Test_GrantNamesAnAgentOfTheTenant() {
	t := s.W.NewTenant(s.T(), "Acme")
	unknown := uuid.New()
	_, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &unknown, Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 1, PolicyRef: "P", Days: 1})
	s.code(err, fault.NotFound)
	s.Require().NoError(s.W.Kernel.Agents.Revoke(t.Owner, t.AgentId))
	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 1, PolicyRef: "P", Days: 1})
	s.code(err, fault.InvalidArgument)
}

// A mandate's days are 24 hours each: the grant's length is exact. The
// second half shows, in Postgres itself, why the grant adds hours and not
// days: across a daylight saving change, days in a local time zone are an
// hour short or long, which the database's check of the length refuses.
func (s *MandatesSuite) Test_ValidityCountsDaysOf24Hours() {
	t := s.W.NewTenant(s.T(), "Acme")
	m := s.grant(t.Owner, mandates.Input{Actions: []string{"*"}, Days: 366})
	s.Require().Equal(366*24*time.Hour, m.ValidUntil.Sub(m.ValidFrom))
	// The same addition in a session in Stockholm, across the change.
	ctx := context.Background()
	tx, err := s.W.Pg.Owner.Begin(ctx)
	s.Require().NoError(err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SET LOCAL TIME ZONE 'Europe/Stockholm'`)
	s.Require().NoError(err)
	// Thirty days from mid March cross the spring change and only that one.
	var hours, days int64
	s.Require().NoError(tx.QueryRow(ctx, `SELECT
		extract(epoch FROM (timestamptz '2026-03-15 12:00+01' + 30 * interval '24 hours') - timestamptz '2026-03-15 12:00+01')::bigint,
		extract(epoch FROM (timestamptz '2026-03-15 12:00+01' + interval '30 days') - timestamptz '2026-03-15 12:00+01')::bigint`).Scan(&hours, &days))
	s.Require().Equal(int64(30*24*3600), hours, "hours do not stretch over the change")
	s.Require().Equal(int64(30*24*3600-3600), days, "days would: why the grant counts hours")
}

// shift moves a mandate's window by an interval, past the guard, as the
// passing of time would.
func (s *MandatesSuite) shift(id uuid.UUID, by string) {
	s.T().Helper()
	_, err := s.W.Pg.Owner.Exec(context.Background(), `BEGIN; SET LOCAL session_replication_role = replica;
		UPDATE mandate.mandates SET valid_from = valid_from + interval '`+by+`', valid_until = valid_until + interval '`+by+`' WHERE id = '`+id.String()+`'; COMMIT;`)
	s.Require().NoError(err)
}

func (s *MandatesSuite) Test_AnotherTenantSeesNoMandates() {
	a := s.W.NewTenant(s.T(), "A")
	b := s.W.NewTenant(s.T(), "B")
	m := s.grant(a.Owner, mandates.Input{Actions: []string{"*"}})
	_, err := s.W.Kernel.Mandates.Grant(b.Owner, mandates.Input{AgentId: &a.AgentId, Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 1, PolicyRef: "P", Days: 1})
	s.code(err, fault.NotFound)
	s.invalid(b.Owner, `INSERT INTO mandate.mandates (id, tenant_id, agent_id, actions, max_risk, budget_count, policy_ref, valid_until)
		VALUES (gen_random_uuid(), $1, $2, '{*}', 'low', 1, 'P', now() + interval '1 day')`, b.Id, a.AgentId)
	list, err := s.W.Kernel.Mandates.List(b.Owner, true)
	s.Require().NoError(err)
	s.Require().Empty(list)
	_, err = s.W.Kernel.Mandates.Get(b.Owner, m.Id)
	s.code(err, fault.NotFound)
	s.code(s.W.Kernel.Mandates.Revoke(b.Owner, m.Id), fault.NotFound)
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(b.System, a.AgentId, "test.low", mandates.RiskLow).Reason)
	s.refused(b.Owner, `INSERT INTO mandate.mandates (id, tenant_id, actions, max_risk, budget_count, policy_ref, valid_until)
		VALUES (gen_random_uuid(), $1, '{*}', 'low', 1, 'P', now() + interval '1 day')`, a.Id)
}
