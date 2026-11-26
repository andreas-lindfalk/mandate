package proposals_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// WithdrawSuite tests an agent taking back its own waiting proposal.
type WithdrawSuite struct {
	IntegrationSuite
}

func TestWithdrawSuite(t *testing.T) {
	suite.Run(t, new(WithdrawSuite))
}

func (s *WithdrawSuite) propose(ctx context.Context, action string) proposals.Proposal {
	s.T().Helper()
	p, _, err := s.W.Kernel.Proposals.Propose(ctx, proposals.Input{IdempotencyKey: uuid.NewString(), Action: action, Title: "x", Payload: json.RawMessage(`{}`), Rationale: "Why."})
	s.Require().NoError(err)
	return p
}

func (s *WithdrawSuite) Test_AnAgentWithdrawsItsOwnWaitingProposal() {
	t := s.W.NewTenant(s.T(), "Acme")
	p := s.propose(t.Agent, "test.manual")
	withdrawn, err := s.W.Kernel.Proposals.Withdraw(t.Agent, p.Id, "  Proposed again with the right order id.  ")
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusWithdrawn, withdrawn.Status)
	s.Require().Equal("Proposed again with the right order id.", withdrawn.DecisionNote)
	s.Require().NotNil(withdrawn.DecidedAt)
	s.Require().Nil(withdrawn.DecidedBy)
	s.Require().Equal([]string{"proposal.created", "proposal.withdrawn"}, s.eventTypes(t.Owner, p.Id))

	_, err = s.W.Kernel.Proposals.Withdraw(t.Agent, p.Id, "")
	s.code(err, fault.FailedPrecondition)
	_, err = s.W.Kernel.Proposals.Approve(t.Reviewer, p.Id, proposals.Decision{})
	s.code(err, fault.FailedPrecondition)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, p.Id), "nothing to execute, and no failure")
	after, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusWithdrawn, after.Status)

	// Only its own, and only while it waits.
	other, err := s.W.Kernel.Agents.Create(t.Owner, "other", "", "live")
	s.Require().NoError(err)
	_, secret, err := s.W.Kernel.Agents.IssueToken(t.Owner, other.Id, 30)
	s.Require().NoError(err)
	grant, err := s.W.Kernel.Agents.Authenticate(context.Background(), secret)
	s.Require().NoError(err)
	q := s.propose(t.Agent, "test.manual")
	_, err = s.W.Kernel.Proposals.Withdraw(grant.Context(context.Background()), q.Id, "")
	s.code(err, fault.NotFound)
	_, err = s.W.Kernel.Proposals.Withdraw(t.Reviewer, q.Id, "")
	s.code(err, fault.PermissionDenied)
	_, err = s.W.Kernel.Proposals.Reject(t.Reviewer, q.Id, "no")
	s.Require().NoError(err)
	_, err = s.W.Kernel.Proposals.Withdraw(t.Agent, q.Id, "")
	s.code(err, fault.FailedPrecondition)

	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"test.low"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	r := s.propose(t.Agent, "test.low")
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, r.Id))
	_, err = s.W.Kernel.Proposals.Withdraw(t.Agent, r.Id, "")
	s.code(err, fault.FailedPrecondition)
}

// The database holds the same rules, whatever the service does.
func (s *WithdrawSuite) Test_TheDatabaseLetsOnlyTheAgentWithdraw() {
	t := s.W.NewTenant(s.T(), "Acme")
	p := s.propose(t.Agent, "test.manual")
	s.refused(t.Owner, `UPDATE mandate.proposals SET status = 'withdrawn' WHERE id = $1`, p.Id)
	s.refused(t.System, `UPDATE mandate.proposals SET status = 'withdrawn' WHERE id = $1`, p.Id)
	s.refused(t.Agent, `UPDATE mandate.proposals SET status = 'withdrawn', payload = '{"more":1}' WHERE id = $1`, p.Id)
	s.refused(t.Agent, `UPDATE mandate.proposals SET status = 'withdrawn', risk = 'high' WHERE id = $1`, p.Id)
	s.refused(t.Agent, `UPDATE mandate.proposals SET status = 'approved' WHERE id = $1`, p.Id)

	// Row level security separates tenants, not agents: another agent in
	// the same tenant reaches the row, and only the trigger stops it.
	other, err := s.W.Kernel.Agents.Create(t.Owner, "other", "", "live")
	s.Require().NoError(err)
	_, secret, err := s.W.Kernel.Agents.IssueToken(t.Owner, other.Id, 30)
	s.Require().NoError(err)
	grant, err := s.W.Kernel.Agents.Authenticate(context.Background(), secret)
	s.Require().NoError(err)
	s.refused(grant.Context(context.Background()), `UPDATE mandate.proposals SET status = 'withdrawn' WHERE id = $1`, p.Id)
	still, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusPending, still.Status)

	s.Require().NoError(s.exec(t.Agent, `UPDATE mandate.proposals SET status = 'withdrawn', decision_note = 'gone' WHERE id = $1`, p.Id))
	s.refused(t.Agent, `UPDATE mandate.proposals SET status = 'pending' WHERE id = $1`, p.Id)
}
