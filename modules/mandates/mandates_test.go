package mandates_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	if input.BudgetUnits == 0 {
		input.BudgetUnits = 10
	}
	if input.MaxRisk == "" {
		input.MaxRisk = mandates.RiskLow
	}
	m, err := s.W.Kernel.Mandates.Grant(ctx, input)
	s.Require().NoError(err)
	return m
}

func (s *MandatesSuite) resolve(ctx context.Context, agentId uuid.UUID, action, risk string, cost int64) mandates.Resolution {
	s.T().Helper()
	var r mandates.Resolution
	s.Require().NoError(db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		var err error
		r, err = s.W.Kernel.Mandates.Resolve(ctx, tx, agentId, action, risk, cost)
		return err
	}))
	return r
}

func (s *MandatesSuite) Test_GrantValidation() {
	t := s.W.NewTenant(s.T(), "Acme")
	ok := mandates.Input{Actions: []string{"tickets.*"}, MaxRisk: mandates.RiskLow, BudgetUnits: 10, PolicyRef: "P", Days: 30}
	for name, change := range map[string]func(*mandates.Input){
		"no actions":      func(i *mandates.Input) { i.Actions = nil },
		"bad pattern":     func(i *mandates.Input) { i.Actions = []string{"Tickets.*"} },
		"high risk":       func(i *mandates.Input) { i.MaxRisk = mandates.RiskHigh },
		"unknown risk":    func(i *mandates.Input) { i.MaxRisk = "none" },
		"zero budget":     func(i *mandates.Input) { i.BudgetUnits = 0 },
		"negative budget": func(i *mandates.Input) { i.BudgetUnits = -1 },
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

func (s *MandatesSuite) Test_OwnerGrantsAndRevokesOnce() {
	t := s.W.NewTenant(s.T(), "Acme")
	m := s.grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"tickets.*", "refunds.issue"}, MaxRisk: mandates.RiskMedium, BudgetUnits: 42, PolicyRef: "ACME-AI-POLICY §4", Days: 90})
	s.Require().Equal(&t.OwnerId, m.GrantedBy)
	s.Require().Equal(&t.AgentId, m.AgentId)
	s.Require().Equal(int64(42), m.BudgetUnits)
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
	input := mandates.Input{Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetUnits: 1, PolicyRef: "P", Days: 1}
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
	insert := `INSERT INTO mandate.mandates (id, tenant_id, agent_id, actions, max_risk, budget_units, policy_ref, valid_until)
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
	s.refused(t.Owner, `UPDATE mandate.mandates SET budget_units = 1000000 WHERE id = $1`, m.Id)
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
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow, 1).Reason)

	other, err := s.W.Kernel.Agents.Create(t.Owner, "other-bot", "")
	s.Require().NoError(err)
	s.grant(t.Owner, mandates.Input{AgentId: &other.Id, Actions: []string{"*"}, MaxRisk: mandates.RiskMedium})
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow, 1).Reason, "another agent's mandate does not cover me")

	low := s.grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.*"}, MaxRisk: mandates.RiskLow, BudgetUnits: 3})
	r := s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow, 1)
	s.Require().Empty(r.Reason)
	s.Require().Equal(low.Id, r.Mandate.Id)
	s.Require().Zero(r.SpentUnits)
	s.Require().Equal(mandates.SkipRiskAbove, s.resolve(t.System, t.AgentId, "test.medium", mandates.RiskMedium, 1).Reason)
	s.Require().Equal(mandates.SkipRiskAbove, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskHigh, 1).Reason)
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "tickets.create", mandates.RiskLow, 1).Reason)
	s.Require().Equal(mandates.SkipBudgetSpent, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow, 4).Reason, "the cost must fit")

	medium := s.grant(t.Owner, mandates.Input{Actions: []string{"test.low"}, MaxRisk: mandates.RiskMedium})
	r = s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow, 1)
	s.Require().Equal(medium.Id, r.Mandate.Id, "the most permissive mandate with room is chosen")
	r = s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow, 5)
	s.Require().Equal(medium.Id, r.Mandate.Id, "the low one has no room for 5; the medium one has")
}

func (s *MandatesSuite) Test_ValidityAndRevocation() {
	t := s.W.NewTenant(s.T(), "Acme")
	m := s.grant(t.Owner, mandates.Input{Actions: []string{"*"}, Days: 10})
	s.Require().Empty(s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow, 1).Reason)

	s.W.SetNow(time.Now().AddDate(0, 0, 11))
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow, 1).Reason, "expired")
	live, err := s.W.Kernel.Mandates.List(t.Owner, false)
	s.Require().NoError(err)
	s.Require().Len(live, 1, "the list is the database's clock; the mandate is still in its window there")
	s.W.SetNow(time.Time{})

	s.Require().NoError(s.W.Kernel.Mandates.Revoke(t.Owner, m.Id))
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(t.System, t.AgentId, "test.low", mandates.RiskLow, 1).Reason, "revoked")
}

func (s *MandatesSuite) Test_GrantNamesAnAgentOfTheTenant() {
	t := s.W.NewTenant(s.T(), "Acme")
	unknown := uuid.New()
	_, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &unknown, Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetUnits: 1, PolicyRef: "P", Days: 1})
	s.code(err, fault.NotFound)
	s.Require().NoError(s.W.Kernel.Agents.Revoke(t.Owner, t.AgentId))
	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetUnits: 1, PolicyRef: "P", Days: 1})
	s.code(err, fault.InvalidArgument)
}

func (s *MandatesSuite) Test_ValidityCountsUtcDays() {
	t := s.W.NewTenant(s.T(), "Acme")
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	s.Require().NoError(err)
	// Across the daylight saving change, 366 local days are one hour more
	// than 366 UTC days; the database checks the exact difference.
	s.W.SetNow(time.Date(2026, 3, 1, 12, 0, 0, 0, stockholm))
	defer s.W.SetNow(time.Time{})
	m := s.grant(t.Owner, mandates.Input{Actions: []string{"*"}, Days: 366})
	s.Require().Equal(366*24*time.Hour, m.ValidUntil.Sub(m.ValidFrom))
}

func (s *MandatesSuite) Test_AnotherTenantSeesNoMandates() {
	a := s.W.NewTenant(s.T(), "A")
	b := s.W.NewTenant(s.T(), "B")
	m := s.grant(a.Owner, mandates.Input{Actions: []string{"*"}})
	_, err := s.W.Kernel.Mandates.Grant(b.Owner, mandates.Input{AgentId: &a.AgentId, Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetUnits: 1, PolicyRef: "P", Days: 1})
	s.code(err, fault.NotFound)
	s.invalid(b.Owner, `INSERT INTO mandate.mandates (id, tenant_id, agent_id, actions, max_risk, budget_units, policy_ref, valid_until)
		VALUES (gen_random_uuid(), $1, $2, '{*}', 'low', 1, 'P', now() + interval '1 day')`, b.Id, a.AgentId)
	list, err := s.W.Kernel.Mandates.List(b.Owner, true)
	s.Require().NoError(err)
	s.Require().Empty(list)
	_, err = s.W.Kernel.Mandates.Get(b.Owner, m.Id)
	s.code(err, fault.NotFound)
	s.code(s.W.Kernel.Mandates.Revoke(b.Owner, m.Id), fault.NotFound)
	s.Require().Equal(mandates.SkipNoMandate, s.resolve(b.System, a.AgentId, "test.low", mandates.RiskLow, 1).Reason)
	s.refused(b.Owner, `INSERT INTO mandate.mandates (id, tenant_id, actions, max_risk, budget_units, policy_ref, valid_until)
		VALUES (gen_random_uuid(), $1, '{*}', 'low', 1, 'P', now() + interval '1 day')`, a.Id)
}
