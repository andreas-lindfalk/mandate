package proposals_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/modules/agents"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// DryRunSuite holds what a dry run may and may not become: recorded, never
// carried out.
type DryRunSuite struct {
	IntegrationSuite
}

func TestDryRunSuite(t *testing.T) {
	suite.Run(t, new(DryRunSuite))
}

func (s *DryRunSuite) propose(ctx context.Context, action, key string) proposals.Proposal {
	s.T().Helper()
	p, _, err := s.W.Kernel.Proposals.Propose(ctx, proposals.Input{Rationale: "Why.", IdempotencyKey: key, Action: action, Title: "T", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	return p
}

func (s *DryRunSuite) grant(t testfix.Tenant, agentId uuid.UUID) mandates.Mandate {
	s.T().Helper()
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &agentId, Actions: []string{"test.*"}, MaxRisk: mandates.RiskMedium, BudgetCount: 1, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	return m
}

func (s *DryRunSuite) status(t testfix.Tenant, id uuid.UUID) string {
	s.T().Helper()
	p, err := s.W.Kernel.Proposals.Get(t.Owner, id)
	s.Require().NoError(err)
	return p.Status
}

// eventFields reads the payloads of the events about a proposal.
func (s *DryRunSuite) eventFields(t testfix.Tenant, id uuid.UUID) map[string]map[string]any {
	s.T().Helper()
	out := map[string]map[string]any{}
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		list, err := events.List(t.Owner, tx, &id, 0)
		for _, e := range list {
			var fields map[string]any
			s.Require().NoError(json.Unmarshal(e.Payload, &fields))
			out[e.Type] = fields
		}
		return err
	}))
	return out
}

// A mandate would allow a dry run: the basis is recorded, nothing is
// applied, and the budget is untouched, so the live agent still has its
// one execution.
func (s *DryRunSuite) Test_AMandateWouldAllowItAndNothingHappens() {
	t := s.W.NewTenant(s.T(), "Acme")
	dry := s.W.DryRunAgent(s.T(), t)
	m := s.grant(t, dry.Id)
	p := s.propose(dry.Agent, "test.low", "k-1")
	s.Require().True(p.DryRun, "the database marks it from the agent's mode")

	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, p.Id))
	after, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusWouldAllow, after.Status)
	s.Require().NotNil(after.ExecutionBasis)
	s.Require().Equal(m.Id, after.ExecutionBasis.MandateId)
	s.Require().NotContains(s.W.Applied(), p.Id, "nothing is applied")
	spent, err := s.W.Kernel.Mandates.Spent(t.Owner, m.Id)
	s.Require().NoError(err)
	s.Require().Zero(spent, "a dry run adds nothing to the budget")

	// The same last place in the budget would allow the next dry run too.
	q := s.propose(dry.Agent, "test.low", "k-2")
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, q.Id))
	s.Require().Equal(proposals.StatusWouldAllow, s.status(t, q.Id))

	// Every event of a dry run says so.
	fields := s.eventFields(t, p.Id)
	s.Require().Equal(true, fields["proposal.created"]["dryRun"])
	s.Require().Equal(true, fields["proposal.would_allow"]["dryRun"])
	s.Require().NotContains(fields, "proposal.executed")
	decided, err := s.W.Kernel.Proposals.Decided(t.Owner)
	s.Require().NoError(err)
	s.Require().Empty(decided, "dry runs are left out of the decisions export")
}

// A person only dismisses a dry run; a live proposal is never dismissed.
func (s *DryRunSuite) Test_APersonOnlyDismissesADryRun() {
	t := s.W.NewTenant(s.T(), "Acme")
	dry := s.W.DryRunAgent(s.T(), t)
	p := s.propose(dry.Agent, "test.manual", "k-1")
	_, err := s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	s.code(err, fault.FailedPrecondition)
	_, err = s.W.Kernel.Proposals.Reject(t.Reviewer, p.Id, "no")
	s.code(err, fault.FailedPrecondition)
	_, err = s.W.Kernel.Proposals.Dismiss(dry.Agent, p.Id, "")
	s.code(err, fault.PermissionDenied)
	dismissed, err := s.W.Kernel.Proposals.Dismiss(t.Reviewer, p.Id, "Seen.")
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusDismissed, dismissed.Status)
	s.Require().Equal("Seen.", dismissed.DecisionNote)
	s.Require().Equal(true, s.eventFields(t, p.Id)["proposal.dismissed"]["dryRun"])
	_, err = s.W.Kernel.Proposals.Dismiss(t.Reviewer, p.Id, "")
	s.code(err, fault.FailedPrecondition)

	live := s.propose(t.Agent, "test.manual", "k-live")
	s.Require().False(live.DryRun)
	_, err = s.W.Kernel.Proposals.Dismiss(t.Owner, live.Id, "")
	s.code(err, fault.FailedPrecondition)
	_, hasDryRun := s.eventFields(t, live.Id)["proposal.created"]["dryRun"]
	s.Require().False(hasDryRun, "a live proposal's events are not tagged")
}

// The database holds the dry run's rules whatever the Go code does.
func (s *DryRunSuite) Test_TheDatabaseHoldsTheDryRun() {
	t := s.W.NewTenant(s.T(), "Acme")
	dry := s.W.DryRunAgent(s.T(), t)
	p := s.propose(dry.Agent, "test.manual", "k-1")
	live := s.propose(t.Agent, "test.manual", "k-2")

	s.refused(t.Owner, `UPDATE mandate.proposals SET status = 'approved' WHERE id = $1`, p.Id)
	s.refused(t.Owner, `UPDATE mandate.proposals SET status = 'rejected' WHERE id = $1`, p.Id)
	s.refused(t.Owner, `UPDATE mandate.proposals SET dry_run = false WHERE id = $1`, p.Id)
	s.refused(t.Owner, `UPDATE mandate.proposals SET dry_run = true WHERE id = $1`, live.Id)
	s.refused(t.Owner, `UPDATE mandate.proposals SET status = 'dismissed' WHERE id = $1`, live.Id)
	s.refused(t.System, `UPDATE mandate.proposals SET status = 'would_allow', execution_basis = '{}' WHERE id = $1`, live.Id)
	s.refused(t.System, `UPDATE mandate.proposals SET status = 'would_allow', execution_basis = '{}' WHERE id = $1`, p.Id)
	s.refused(dry.Agent, `UPDATE mandate.proposals SET status = 'dismissed' WHERE id = $1`, p.Id)
	s.refused(t.Owner, `UPDATE mandate.proposals SET status = 'dismissed', title = 'changed' WHERE id = $1`, p.Id)
	// The agent cannot choose: the insert trigger sets it from its mode.
	s.Require().NoError(s.exec(t.Agent, `INSERT INTO mandate.proposals (id, tenant_id, agent_id, action, risk, title, payload, idempotency_key, request_hash, rationale, dry_run)
		VALUES (gen_random_uuid(), $1, $2, 'test.manual', 'low', 'x', '{}', 'planted', sha256('planted'), 'Why.', true)`, t.Id, t.AgentId))
	var plantedDryRun bool
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Owner, `SELECT dry_run FROM mandate.proposals WHERE idempotency_key = 'planted'`).Scan(&plantedDryRun)
	}))
	s.Require().False(plantedDryRun, "a live agent's proposal is live, whatever the insert said")

	// No host table opens for a dry run, even as the system inside its
	// own execution.
	var may bool
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.System, `SELECT set_config('mandate.executing_proposal', $1, true)`, p.Id.String()); err != nil {
			return err
		}
		return tx.QueryRow(t.System, `SELECT mandate.may_write_for($1)`, p.Id).Scan(&may)
	}))
	s.Require().False(may)
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.System, `SELECT set_config('mandate.executing_proposal', $1, true)`, live.Id.String()); err != nil {
			return err
		}
		return tx.QueryRow(t.System, `SELECT mandate.may_write_for($1)`, live.Id).Scan(&may)
	}))
	s.Require().True(may, "the same check opens it for a live proposal")
	unseen := uuid.New()
	s.Require().NoError(db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.System, `SELECT set_config('mandate.executing_proposal', $1, true)`, unseen.String()); err != nil {
			return err
		}
		return tx.QueryRow(t.System, `SELECT mandate.may_write_for($1)`, unseen).Scan(&may)
	}))
	s.Require().False(may, "a proposal it cannot see opens nothing")

	// A dismissal, by a person, changes only the status and the note.
	s.Require().NoError(s.exec(t.Reviewer, `UPDATE mandate.proposals SET status = 'dismissed', decision_note = 'seen' WHERE id = $1`, p.Id))
	s.Require().Equal(proposals.StatusDismissed, s.status(t, p.Id))
}

// Switching an agent's mode changes what it proposes from then on, never
// what it already proposed; a key reused across the switch returns the
// first proposal as it was.
func (s *DryRunSuite) Test_TheModeIsFixedOnEachProposal() {
	t := s.W.NewTenant(s.T(), "Acme")
	dry := s.W.DryRunAgent(s.T(), t)
	before := s.propose(dry.Agent, "test.manual", "k-1")
	agent, err := s.W.Kernel.Agents.SetMode(t.Owner, dry.Id, agents.ModeLive)
	s.Require().NoError(err)
	s.Require().Equal(agents.ModeLive, agent.Mode)
	_, err = s.W.Kernel.Agents.SetMode(t.Reviewer, dry.Id, agents.ModeDryRun)
	s.code(err, fault.PermissionDenied)
	after := s.propose(dry.Agent, "test.manual", "k-2")
	s.Require().True(before.DryRun)
	s.Require().False(after.DryRun)
	again, created, err := s.W.Kernel.Proposals.Propose(dry.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: "k-1", Action: "test.manual", Title: "T", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	s.Require().False(created)
	s.Require().True(again.DryRun, "the repeat is the dry run it was")
	s.Require().Contains(s.eventTypes(t.Owner, dry.Id), "agent.mode_changed")
	// Setting the same mode again changes nothing and logs nothing.
	_, err = s.W.Kernel.Agents.SetMode(t.Owner, dry.Id, agents.ModeLive)
	s.Require().NoError(err)
	modeChanges := 0
	for _, e := range s.eventTypes(t.Owner, dry.Id) {
		if e == "agent.mode_changed" {
			modeChanges++
		}
	}
	s.Require().Equal(1, modeChanges)
}

// A waiting dry run tells no chat: nobody is to decide it.
func (s *DryRunSuite) Test_ADryRunTellsNobody() {
	t := s.W.NewTenant(s.T(), "Acme")
	dry := s.W.DryRunAgent(s.T(), t)
	var told []uuid.UUID
	s.W.Kernel.Proposals.SetOnWaiting(func(_ context.Context, _ pgx.Tx, id uuid.UUID) error {
		told = append(told, id)
		return nil
	})
	defer s.W.Kernel.Proposals.SetOnWaiting(s.W.Kernel.Notifications.Enqueue)
	p := s.propose(dry.Agent, "test.manual", "k-1")
	s.Require().NoError(s.W.Kernel.Proposals.RecordSkip(t.System, p.Id, "refused_test"))
	live := s.propose(t.Agent, "test.manual", "k-2")
	s.Require().Equal([]uuid.UUID{live.Id}, told)

	// A proposal that is gone, as with its tenant, has nothing to record.
	gone := uuid.New()
	s.Require().NoError(s.W.Kernel.Proposals.RecordSkip(t.System, gone, "refused_test"))
	s.Require().Empty(s.eventTypes(t.Owner, gone))
}
