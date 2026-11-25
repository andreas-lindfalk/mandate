package proposals_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// LimitsSuite tests the limits per agent that the database holds.
type LimitsSuite struct {
	IntegrationSuite
}

func TestLimitsSuite(t *testing.T) {
	suite.Run(t, new(LimitsSuite))
}

func (s *LimitsSuite) input(key string) proposals.Input {
	return proposals.Input{IdempotencyKey: key, Action: "test.low", Title: "x", Payload: json.RawMessage(`{}`), Rationale: "Why."}
}

func (s *LimitsSuite) Test_AnAgentBeyondItsLimitIsRefused() {
	t := s.W.NewTenant(s.T(), "Acme")
	agent, err := s.W.Kernel.Agents.Get(t.Owner, t.AgentId)
	s.Require().NoError(err)
	s.Require().Equal(30, agent.MaxPerMinute, "the default")
	s.Require().Equal(300, agent.MaxPerHour)
	_, err = s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, 2, 3)
	s.Require().NoError(err)

	_, _, err = s.W.Kernel.Proposals.Propose(t.Agent, s.input("k1"))
	s.Require().NoError(err)
	_, _, err = s.W.Kernel.Proposals.Propose(t.Agent, s.input("k2"))
	s.Require().NoError(err)
	_, _, err = s.W.Kernel.Proposals.Propose(t.Agent, s.input("k3"))
	s.code(err, fault.RateLimited)
	s.Require().Contains(err.Error(), "2 proposals a minute")
	var pgErr *pgconn.PgError
	s.Require().ErrorAs(err, &pgErr)
	seconds, convErr := strconv.Atoi(pgErr.Hint)
	s.Require().NoError(convErr, "the hint is the seconds until a place opens")
	s.Require().True(seconds >= 1 && seconds <= 60, seconds)
	// A repeat inserts nothing: it is answered, and not counted.
	_, created, err := s.W.Kernel.Proposals.Propose(t.Agent, s.input("k1"))
	s.Require().NoError(err)
	s.Require().False(created)

	// The hour counts too: the two are moved half an hour back, out of the
	// minute but within the hour.
	_, err = s.W.Pg.Owner.Exec(context.Background(), `
		BEGIN;
		SET LOCAL session_replication_role = replica;
		UPDATE mandate.proposals SET created_at = now() - interval '30 minutes' WHERE agent_id = '`+t.AgentId.String()+`';
		COMMIT;`)
	s.Require().NoError(err)
	_, _, err = s.W.Kernel.Proposals.Propose(t.Agent, s.input("k4"))
	s.Require().NoError(err)
	_, _, err = s.W.Kernel.Proposals.Propose(t.Agent, s.input("k5"))
	s.code(err, fault.RateLimited)
	s.Require().Contains(err.Error(), "3 proposals an hour")
	s.Require().ErrorAs(err, &pgErr)
	seconds, convErr = strconv.Atoi(pgErr.Hint)
	s.Require().NoError(convErr)
	s.Require().True(seconds > 60 && seconds <= 30*60, "the oldest of the hour leaves it in about half an hour: %d", seconds)

	// Another agent of the same tenant has its own count.
	other, err := s.W.Kernel.Agents.Create(t.Owner, "other", "", "live")
	s.Require().NoError(err)
	_, secret, err := s.W.Kernel.Agents.IssueToken(t.Owner, other.Id, 30)
	s.Require().NoError(err)
	grant, err := s.W.Kernel.Agents.Authenticate(context.Background(), secret)
	s.Require().NoError(err)
	_, _, err = s.W.Kernel.Proposals.Propose(grant.Context(context.Background()), s.input("k1"))
	s.Require().NoError(err)
}

// Proposals made at once are counted one after the other: the limit is
// never passed by a race.
func (s *LimitsSuite) Test_TheLimitHoldsUnderConcurrency() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, 3, 100)
	s.Require().NoError(err)
	const racers = 10
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = s.W.Kernel.Proposals.Propose(t.Agent, s.input(uuid.NewString()))
		}()
	}
	wg.Wait()
	accepted := 0
	for _, err := range errs {
		if err == nil {
			accepted++
		} else {
			s.code(err, fault.RateLimited)
		}
	}
	s.Require().Equal(3, accepted)
}

// A repeat that arrives while its first copy takes the last place gets the
// first answer, not a refusal: it waited on the count, and found itself.
func (s *LimitsSuite) Test_ARepeatAtTheLimitIsStillARepeat() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, 2, 100)
	s.Require().NoError(err)
	_, _, err = s.W.Kernel.Proposals.Propose(t.Agent, s.input(uuid.NewString()))
	s.Require().NoError(err)
	const copies = 8
	key := uuid.NewString()
	ids := make([]uuid.UUID, copies)
	errs := make([]error, copies)
	var wg sync.WaitGroup
	for i := range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var p proposals.Proposal
			p, _, errs[i] = s.W.Kernel.Proposals.Propose(t.Agent, s.input(key))
			ids[i] = p.Id
		}()
	}
	wg.Wait()
	for i, err := range errs {
		s.Require().NoError(err, "copy %d", i)
		s.Require().Equal(ids[0], ids[i])
	}
	_, _, err = s.W.Kernel.Proposals.Propose(t.Agent, s.input(uuid.NewString()))
	s.code(err, fault.RateLimited)
}

func (s *LimitsSuite) Test_OnlyAnOwnerSetsLimits() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Agents.SetLimits(t.Reviewer, t.AgentId, 5, 50)
	s.code(err, fault.PermissionDenied)
	for _, limits := range [][2]int{{0, 10}, {10, 5}, {10_001, 20_000}, {5, 100_001}} {
		_, err := s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, limits[0], limits[1])
		s.code(err, fault.InvalidArgument)
	}
	set, err := s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, 5, 50)
	s.Require().NoError(err)
	s.Require().Equal(5, set.MaxPerMinute)
	s.Require().Contains(s.eventTypes(t.Owner, t.AgentId), "agent.limits_set")

	// The database holds the same, whatever the service does.
	s.refused(t.Reviewer, `UPDATE mandate.agents SET max_per_minute = 1000 WHERE id = $1`, t.AgentId)
	s.invalid(t.Owner, `UPDATE mandate.agents SET max_per_minute = 60, max_per_hour = 50 WHERE id = $1`, t.AgentId)
	s.refused(t.Owner, `UPDATE mandate.agents SET max_per_minute = 6, name = 'renamed' WHERE id = $1`, t.AgentId)
	s.Require().NoError(s.W.Kernel.Agents.Revoke(t.Owner, t.AgentId))
	_, err = s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, 5, 50)
	s.code(err, fault.NotFound)
	s.refused(t.Owner, `UPDATE mandate.agents SET max_per_minute = 6 WHERE id = $1`, t.AgentId)
}
