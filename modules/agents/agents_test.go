package agents_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/modules/agents"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type AgentsSuite struct {
	IntegrationSuite
}

func TestAgentsSuite(t *testing.T) {
	suite.Run(t, new(AgentsSuite))
}

func (s *AgentsSuite) Test_OwnerRegistersAgentsAndIssuesTokens() {
	t := s.W.NewTenant(s.T(), "Acme")
	agent, err := s.W.Kernel.Agents.Create(t.Owner, "triage-bot", "", "live")
	s.Require().NoError(err)
	s.Require().Equal(&t.OwnerId, agent.CreatedBy)
	s.Require().Equal([]string{"agent.created"}, s.eventTypes(t.Owner, agent.Id))

	token, secret, err := s.W.Kernel.Agents.IssueToken(t.Owner, agent.Id, 10)
	s.Require().NoError(err)
	s.Require().True(strings.HasPrefix(secret, agents.TokenPrefix))
	s.Require().True(strings.HasSuffix(secret, token.Hint))

	grant, err := s.W.Kernel.Agents.Authenticate(context.Background(), secret)
	s.Require().NoError(err)
	s.Require().Equal(agent.Id, grant.AgentId)
	s.Require().Equal(t.Id, grant.TenantId)
	s.Require().Equal(token.Id, grant.TokenId)
	a, ok := actor.From(grant.Context(context.Background()))
	s.Require().True(ok)
	s.Require().Equal(actor.Actor{Kind: actor.Agent, Id: agent.Id}, a)

	tokens, err := s.W.Kernel.Agents.ListTokens(t.Owner, agent.Id)
	s.Require().NoError(err)
	s.Require().Len(tokens, 1)
	s.Require().NotNil(tokens[0].LastUsedAt, "authenticating marks the token as used")
	s.Require().Equal([]string{"agent.created", "agent.token_issued"}, s.eventTypes(t.Owner, agent.Id))

	_, _, err = s.W.Kernel.Agents.IssueToken(t.Owner, agent.Id, 400)
	s.code(err, fault.InvalidArgument)
	_, err = s.W.Kernel.Agents.Create(t.Owner, "", "", "live")
	s.code(err, fault.InvalidArgument)
}

func (s *AgentsSuite) Test_OnlyOwners() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Agents.Create(t.Reviewer, "bot", "", "live")
	s.code(err, fault.PermissionDenied)
	_, err = s.W.Kernel.Agents.Create(t.Agent, "bot", "", "live")
	s.code(err, fault.PermissionDenied)
	_, _, err = s.W.Kernel.Agents.IssueToken(t.Reviewer, t.AgentId, 10)
	s.code(err, fault.PermissionDenied)
	s.code(s.W.Kernel.Agents.Revoke(t.Reviewer, t.AgentId), fault.PermissionDenied)
	s.code(s.W.Kernel.Agents.RevokeToken(t.Agent, t.Grant.TokenId), fault.PermissionDenied)
}

// Two revocations of the same token can both read it as live. The one
// whose update then finds it revoked is refused, as a revocation after a
// revocation is, and records nothing.
func (s *AgentsSuite) Test_ARevocationThatLosesTheRaceIsRefused() {
	t := s.W.NewTenant(s.T(), "Acme")
	err := s.W.CommitUnder(s.T(), t.Owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Owner, `UPDATE mandate.agent_tokens SET revoked_at = now() WHERE id = $1`, t.Grant.TokenId)
		return err
	}, "name: RevokeToken ", func() error {
		return s.W.Kernel.Agents.RevokeToken(t.Owner, t.Grant.TokenId)
	})
	s.code(err, fault.Conflict)
	s.Require().Equal([]string{"agent.created", "agent.token_issued"}, s.eventTypes(t.Owner, t.AgentId), "the refused revocation is not in the chain")
	_, err = s.W.Kernel.Agents.Authenticate(context.Background(), t.Token)
	s.code(err, fault.Unauthenticated)
}

func (s *AgentsSuite) Test_TokensStopWorking() {
	t := s.W.NewTenant(s.T(), "Acme")
	ctx := context.Background()
	s.Require().NoError(s.W.Kernel.Agents.RevokeToken(t.Owner, t.Grant.TokenId))
	_, err := s.W.Kernel.Agents.Authenticate(ctx, t.Token)
	s.code(err, fault.Unauthenticated)
	s.code(s.W.Kernel.Agents.RevokeToken(t.Owner, t.Grant.TokenId), fault.Conflict)
	s.code(s.W.Kernel.Agents.RevokeToken(t.Reviewer, t.Grant.TokenId), fault.PermissionDenied)
	s.Require().Equal([]string{"agent.created", "agent.token_issued", "agent.token_revoked"}, s.eventTypes(t.Owner, t.AgentId))

	_, secret, err := s.W.Kernel.Agents.IssueToken(t.Owner, t.AgentId, 10)
	s.Require().NoError(err)
	_, err = s.W.Kernel.Agents.Authenticate(ctx, secret)
	s.Require().NoError(err)

	// Expiry is checked against the database's clock, so the token is
	// issued with a clock eleven days back and ten days of validity.
	back := time.Now().AddDate(0, 0, -11).UTC().Truncate(time.Second)
	s.W.SetNow(back)
	stamped, expired, err := s.W.Kernel.Agents.IssueToken(t.Owner, t.AgentId, 10)
	s.W.SetNow(time.Time{})
	s.Require().NoError(err)
	s.Require().True(back.Equal(stamped.CreatedAt), "the clock stamps the token's creation")
	s.Require().True(back.AddDate(0, 0, 10).Equal(stamped.ExpiresAt), "and its expiry")
	_, err = s.W.Kernel.Agents.Authenticate(ctx, expired)
	s.code(err, fault.Unauthenticated)

	s.Require().NoError(s.W.Kernel.Agents.Revoke(t.Owner, t.AgentId))
	_, err = s.W.Kernel.Agents.Authenticate(ctx, secret)
	s.code(err, fault.Unauthenticated)
	_, _, err = s.W.Kernel.Agents.IssueToken(t.Owner, t.AgentId, 10)
	s.code(err, fault.PermissionDenied)
	agent, err := s.W.Kernel.Agents.Get(t.Owner, t.AgentId)
	s.Require().NoError(err)
	s.Require().NotNil(agent.RevokedAt)

	for _, garbage := range []string{"", "mnd_", "Bearer x", secret + "x", strings.Repeat("mnd_a", 30)} {
		_, err := s.W.Kernel.Agents.Authenticate(ctx, garbage)
		s.code(err, fault.Unauthenticated)
	}
}

func (s *AgentsSuite) Test_AgentRowsCannotBeChanged() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.refused(t.Owner, `UPDATE mandate.agents SET name = 'other' WHERE id = $1`, t.AgentId)
	s.refused(t.Owner, `DELETE FROM mandate.agents WHERE id = $1`, t.AgentId)
	s.refused(t.Owner, `UPDATE mandate.agent_tokens SET expires_at = now() + interval '10 years' WHERE id = $1`, t.Grant.TokenId)
	s.refused(t.Agent, `UPDATE mandate.agents SET revoked_at = now() WHERE id = $1`, t.AgentId)
	s.refused(t.Reviewer, `INSERT INTO mandate.agents (id, tenant_id, name) VALUES (gen_random_uuid(), $1, 'mine')`, t.Id)
	s.Require().NoError(s.W.Kernel.Agents.Revoke(t.Owner, t.AgentId))
	s.refused(t.Owner, `UPDATE mandate.agents SET revoked_at = now() WHERE id = $1`, t.AgentId)
	s.refused(t.Owner, `UPDATE mandate.agents SET revoked_at = NULL WHERE id = $1`, t.AgentId)
}

func (s *AgentsSuite) Test_AnotherTenantSeesNoAgents() {
	a := s.W.NewTenant(s.T(), "A")
	b := s.W.NewTenant(s.T(), "B")
	list, err := s.W.Kernel.Agents.List(b.Owner)
	s.Require().NoError(err)
	s.Require().Len(list, 1)
	s.Require().Equal(b.AgentId, list[0].Id)
	_, err = s.W.Kernel.Agents.Get(b.Owner, a.AgentId)
	s.code(err, fault.NotFound)
	_, _, err = s.W.Kernel.Agents.IssueToken(b.Owner, a.AgentId, 10)
	s.code(err, fault.NotFound)
	s.code(s.W.Kernel.Agents.Revoke(b.Owner, a.AgentId), fault.NotFound)
	s.refused(b.Owner, `INSERT INTO mandate.agents (id, tenant_id, name) VALUES (gen_random_uuid(), $1, 'planted')`, a.Id)
	tokens, err := s.W.Kernel.Agents.ListTokens(b.Owner, a.AgentId)
	s.Require().NoError(err)
	s.Require().Empty(tokens)
	s.code(s.W.Kernel.Agents.RevokeToken(b.Owner, a.Grant.TokenId), fault.NotFound)
	s.Require().NoError(s.exec(b.Owner, `UPDATE mandate.agent_tokens SET revoked_at = now() WHERE id = $1`, a.Grant.TokenId), "invisible rows are not updated")
	_, err = s.W.Kernel.Agents.Authenticate(context.Background(), a.Token)
	s.Require().NoError(err, "A's token still works")
	grant, err := s.W.Kernel.Agents.Authenticate(context.Background(), a.Token)
	s.Require().NoError(err)
	s.Require().Equal(a.Id, grant.TenantId, "a token binds to its own tenant")
}

func (s *AgentsSuite) Test_CalledWritesTheChainAsTheAgent() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.Require().NoError(s.W.Kernel.Agents.Called(t.Grant.Context(context.Background()), t.Grant, "propose", map[string]any{"action": "tickets.create"}, false))
	s.Require().Equal([]string{"agent.created", "agent.token_issued", "agent.tool_called"}, s.eventTypes(t.Owner, t.AgentId))
}

// A new agent starts in dry run, and only an owner changes the mode of an
// agent that is not revoked, whatever SQL anyone runs.
func (s *AgentsSuite) Test_TheModeIsAnOwnersToSet() {
	t := s.W.NewTenant(s.T(), "Acme")
	fresh, err := s.W.Kernel.Agents.Create(t.Owner, "new-bot", "", "")
	s.Require().NoError(err)
	s.Require().Equal(agents.ModeDryRun, fresh.Mode, "a new agent starts in dry run")
	_, err = s.W.Kernel.Agents.Create(t.Owner, "odd-bot", "", "sometimes")
	s.code(err, fault.InvalidArgument)

	s.refused(t.Reviewer, `UPDATE mandate.agents SET mode = 'live' WHERE id = $1`, fresh.Id)
	s.refused(t.Agent, `UPDATE mandate.agents SET mode = 'live' WHERE id = $1`, fresh.Id)
	s.Require().NoError(s.exec(t.Owner, `UPDATE mandate.agents SET mode = 'live' WHERE id = $1`, fresh.Id))
	s.Require().Error(s.exec(t.Owner, `UPDATE mandate.agents SET mode = 'sometimes' WHERE id = $1`, fresh.Id), "the table holds the modes")

	other := s.W.NewTenant(s.T(), "Other")
	s.Require().NoError(s.exec(other.Owner, `UPDATE mandate.agents SET mode = 'dry_run' WHERE id = $1`, fresh.Id), "another tenant's agent is out of sight")
	still, err := s.W.Kernel.Agents.Get(t.Owner, fresh.Id)
	s.Require().NoError(err)
	s.Require().Equal(agents.ModeLive, still.Mode)

	s.Require().NoError(s.W.Kernel.Agents.Revoke(t.Owner, fresh.Id))
	s.refused(t.Owner, `UPDATE mandate.agents SET mode = 'dry_run' WHERE id = $1`, fresh.Id)
	_, err = s.W.Kernel.Agents.SetMode(t.Owner, fresh.Id, agents.ModeDryRun)
	s.code(err, fault.NotFound)
	s.Require().ErrorContains(err, "no agent that is not revoked with that id")
}

// A revoked agent's limits are refused in the same words as its mode, never
// with "live", which names a mode.
func (s *AgentsSuite) Test_ARevokedAgentsLimitsAreRefusedAsItsMode() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.Require().NoError(s.W.Kernel.Agents.Revoke(t.Owner, t.AgentId))
	_, err := s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, 5, 50)
	s.code(err, fault.NotFound)
	s.Require().ErrorContains(err, "no agent that is not revoked with that id")
	s.Require().NotContains(err.Error(), "live")
}
