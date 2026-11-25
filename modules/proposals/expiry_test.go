package proposals_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// ExpirySuite tests the deadline a proposal waits under: after it, nobody
// decides it and the system expires it; before it, nothing expires it.
type ExpirySuite struct {
	IntegrationSuite
}

func TestExpirySuite(t *testing.T) {
	suite.Run(t, new(ExpirySuite))
}

func (s *ExpirySuite) propose(ctx context.Context, action string) proposals.Proposal {
	s.T().Helper()
	p, _, err := s.W.Kernel.Proposals.Propose(ctx, proposals.Input{IdempotencyKey: uuid.NewString(), Action: action, Title: "x", Payload: json.RawMessage(`{}`), Rationale: "Why."})
	s.Require().NoError(err)
	return p
}

// pastDeadline moves a proposal's deadline into the past, as the owner of
// the tables and with the triggers set aside, so a test need not wait.
func (s *ExpirySuite) pastDeadline(id uuid.UUID) {
	s.T().Helper()
	_, err := s.W.Pg.Owner.Exec(context.Background(), `
		BEGIN;
		SET LOCAL session_replication_role = replica;
		UPDATE mandate.proposals SET expires_at = now() - interval '1 second' WHERE id = '`+id.String()+`';
		COMMIT;`)
	s.Require().NoError(err)
}

func (s *ExpirySuite) status(t testfix.Tenant, id uuid.UUID) string {
	s.T().Helper()
	p, err := s.W.Kernel.Proposals.Get(t.Owner, id)
	s.Require().NoError(err)
	return p.Status
}

func (s *ExpirySuite) Test_TheDeadlineComesFromTheAction() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "notes.add", Risk: mandates.RiskLow, Connector: actions.ConnectorWebhook, Url: "https://connector.example/hooks",
		Schema: json.RawMessage(`{"type":"object"}`), WaitLimit: 2 * time.Hour, DeliverWithin: 10 * time.Minute,
	})
	s.Require().NoError(err)
	p := s.propose(t.Agent, "notes.add")
	s.Require().WithinDuration(time.Now().Add(2*time.Hour), p.ExpiresAt, time.Minute)
	s.Require().Equal(10*time.Minute, p.DeliverWithin)

	// A later change to the action changes nothing for a proposal made
	// before it.
	_, err = s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "notes.add", Risk: mandates.RiskLow, Connector: actions.ConnectorWebhook, Url: "https://connector.example/hooks",
		Schema: json.RawMessage(`{"type":"object"}`), WaitLimit: 24 * time.Hour, DeliverWithin: time.Hour,
	})
	s.Require().NoError(err)
	again, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(p.ExpiresAt, again.ExpiresAt)
	s.Require().Equal(10*time.Minute, again.DeliverWithin)

	// An action that exists only as code waits the default.
	coded := s.propose(t.Agent, "test.low")
	s.Require().WithinDuration(time.Now().Add(actions.DefaultWaitLimit), coded.ExpiresAt, time.Minute)
	s.Require().Equal(actions.DefaultDeliverWithin, coded.DeliverWithin)
}

func (s *ExpirySuite) Test_AfterTheDeadlineNobodyDecides() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"test.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	p := s.propose(t.Agent, "test.manual")
	s.pastDeadline(p.Id)

	_, err = s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	s.code(err, fault.FailedPrecondition)
	s.Require().Contains(err.Error(), "expired")
	_, err = s.W.Kernel.Proposals.Reject(t.Owner, p.Id, "Too late.")
	s.code(err, fault.FailedPrecondition)
	_, err = s.W.Kernel.Proposals.Withdraw(t.Agent, p.Id, "Too late.")
	s.code(err, fault.FailedPrecondition)

	// The database refuses it too, whatever the service does.
	s.refused(t.Owner, `UPDATE mandate.proposals SET status = 'approved' WHERE id = $1`, p.Id)
	s.refused(t.Owner, `UPDATE mandate.proposals SET status = 'rejected' WHERE id = $1`, p.Id)
	s.refused(t.Agent, `UPDATE mandate.proposals SET status = 'withdrawn' WHERE id = $1`, p.Id)
	s.refused(t.Owner, `UPDATE mandate.proposals SET status = 'expired' WHERE id = $1`, p.Id)
	s.refused(t.System, `UPDATE mandate.proposals SET status = 'expired', title = 'changed' WHERE id = $1`, p.Id)

	// A mandate does not execute it either.
	executable := s.propose(t.Agent, "test.low")
	s.pastDeadline(executable.Id)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, executable.Id))
	s.Require().Equal(proposals.StatusPending, s.status(t, executable.Id))
	s.Require().NotContains(s.W.Applied(), executable.Id)

	// The system expires both, and says so in the log.
	expired, err := s.W.Kernel.Proposals.ExpireDue(t.System)
	s.Require().NoError(err)
	s.Require().Equal(2, expired)
	s.Require().Equal(proposals.StatusExpired, s.status(t, p.Id))
	s.Require().Equal(proposals.StatusExpired, s.status(t, executable.Id))
	s.Require().Equal([]string{"proposal.created", "proposal.expired"}, s.eventTypes(t.Owner, p.Id))
	done, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().NotNil(done.DecidedAt)
	s.Require().Nil(done.DecidedBy)

	// Expired is final, and a second sweep finds nothing.
	s.refused(t.Owner, `UPDATE mandate.proposals SET status = 'approved' WHERE id = $1`, p.Id)
	again, err := s.W.Kernel.Proposals.ExpireDue(t.System)
	s.Require().NoError(err)
	s.Require().Zero(again)
	s.verifyChain(t.Owner)
}

func (s *ExpirySuite) Test_BeforeTheDeadlineNothingExpires() {
	t := s.W.NewTenant(s.T(), "Acme")
	p := s.propose(t.Agent, "test.manual")
	s.refused(t.System, `UPDATE mandate.proposals SET status = 'expired' WHERE id = $1`, p.Id)
	expired, err := s.W.Kernel.Proposals.ExpireDue(t.System)
	s.Require().NoError(err)
	s.Require().Zero(expired)
	s.Require().Equal(proposals.StatusPending, s.status(t, p.Id))

	// A decision before the deadline is in time.
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	s.Require().NoError(err)
}

func (s *ExpirySuite) Test_OnlyTheSystemExpires() {
	t := s.W.NewTenant(s.T(), "Acme")
	p := s.propose(t.Agent, "test.manual")
	s.pastDeadline(p.Id)
	_, err := s.W.Kernel.Proposals.ExpireDue(t.Owner)
	s.code(err, fault.PermissionDenied)
	_, err = s.W.Kernel.Proposals.ExpireDue(t.Agent)
	s.code(err, fault.PermissionDenied)
	s.refused(t.Agent, `UPDATE mandate.proposals SET status = 'expired' WHERE id = $1`, p.Id)
	s.Require().Equal(proposals.StatusPending, s.status(t, p.Id))
}

func (s *ExpirySuite) Test_TheSweepStaysInItsTenant() {
	a := s.W.NewTenant(s.T(), "Acme")
	b := s.W.NewTenant(s.T(), "Bolt")
	pa := s.propose(a.Agent, "test.manual")
	pb := s.propose(b.Agent, "test.manual")
	s.pastDeadline(pa.Id)
	s.pastDeadline(pb.Id)
	expired, err := s.W.Kernel.Proposals.ExpireDue(a.System)
	s.Require().NoError(err)
	s.Require().Equal(1, expired)
	s.Require().Equal(proposals.StatusExpired, s.status(a, pa.Id))
	s.Require().Equal(proposals.StatusPending, s.status(b, pb.Id), "another tenant's proposal is left alone")

	// The kernel sweeps every tenant with work due.
	total, err := s.W.Kernel.ExpireDue(context.Background())
	s.Require().NoError(err)
	s.Require().Equal(1, total)
	s.Require().Equal(proposals.StatusExpired, s.status(b, pb.Id))
}

func (s *ExpirySuite) Test_ExpiredProposalsAreNotDecisions() {
	t := s.W.NewTenant(s.T(), "Acme")
	expired := s.propose(t.Agent, "test.manual")
	s.pastDeadline(expired.Id)
	_, err := s.W.Kernel.Proposals.ExpireDue(t.System)
	s.Require().NoError(err)
	withdrawn := s.propose(t.Agent, "test.manual")
	_, err = s.W.Kernel.Proposals.Withdraw(t.Agent, withdrawn.Id, "")
	s.Require().NoError(err)
	rejected := s.propose(t.Agent, "test.manual")
	_, err = s.W.Kernel.Proposals.Reject(t.Owner, rejected.Id, "No.")
	s.Require().NoError(err)

	decided, err := s.W.Kernel.Proposals.Decided(t.Owner)
	s.Require().NoError(err)
	s.Require().Len(decided, 1, "the decisions export holds what people and mandates decided, not what expired or was withdrawn")
	s.Require().Equal(rejected.Id, decided[0].Id)
}

func (s *ExpirySuite) Test_PastTheDeadlineItReadsAsExpiredBeforeTheSweep() {
	t := s.W.NewTenant(s.T(), "Acme")
	p := s.propose(t.Agent, "test.manual")
	s.pastDeadline(p.Id)
	got, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusPending, got.Status, "the row still waits for the sweep")
	s.Require().True(got.PastDeadline)
	s.Require().Equal(proposals.StatusExpired, got.ShownStatus(), "but nobody can decide it, and it says so")
	listed, err := s.W.Kernel.Proposals.List(t.Owner, proposals.Filter{Status: proposals.StatusPending})
	s.Require().NoError(err)
	s.Require().Len(listed, 1)
	s.Require().Equal(proposals.StatusExpired, listed[0].ShownStatus())

	inTime := s.propose(t.Agent, "test.manual")
	fresh, err := s.W.Kernel.Proposals.Get(t.Owner, inTime.Id)
	s.Require().NoError(err)
	s.Require().False(fresh.PastDeadline)
	s.Require().Equal(proposals.StatusPending, fresh.ShownStatus())
}

// A person deciding and the sweep expiring at the same moment: exactly one
// of them wins, and the log names exactly one outcome.
func (s *ExpirySuite) Test_ADecisionAndTheSweepHaveOneWinner() {
	for round := 0; round < 10; round++ {
		t := s.W.NewTenant(s.T(), "Acme")
		p := s.propose(t.Agent, "test.manual")
		// The deadline is a moment away, so the decision and the sweep
		// race across it.
		_, err := s.W.Pg.Owner.Exec(context.Background(), `
			BEGIN;
			SET LOCAL session_replication_role = replica;
			UPDATE mandate.proposals SET expires_at = now() + interval '20 milliseconds' WHERE id = '`+p.Id.String()+`';
			COMMIT;`)
		s.Require().NoError(err)
		var wg sync.WaitGroup
		var approveErr, expireErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			time.Sleep(15 * time.Millisecond)
			_, approveErr = s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
		}()
		go func() {
			defer wg.Done()
			time.Sleep(25 * time.Millisecond)
			_, expireErr = s.W.Kernel.Proposals.ExpireDue(t.System)
		}()
		wg.Wait()
		s.Require().NoError(expireErr)
		final := s.status(t, p.Id)
		s.Require().Contains([]string{proposals.StatusApproved, proposals.StatusExpired}, final)
		if final == proposals.StatusApproved {
			s.Require().NoError(approveErr)
		} else {
			s.code(approveErr, fault.FailedPrecondition)
		}
		outcomes := 0
		for _, e := range s.eventTypes(t.Owner, p.Id) {
			if e == "proposal.approved" || e == "proposal.expired" {
				outcomes++
			}
		}
		s.Require().Equal(1, outcomes, "one outcome in the log")
	}
}
