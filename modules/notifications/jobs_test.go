package notifications_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/notifications"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
)

// JobsSuite runs the notifications through River, as the server does.
type JobsSuite struct {
	suite.Suite
	W *testfix.World
}

func TestJobsSuite(t *testing.T) {
	suite.Run(t, new(JobsSuite))
}

func (s *JobsSuite) SetupSuite() {
	w, err := testfix.Start(context.Background(), true)
	s.Require().NoError(err)
	s.W = w
}

func (s *JobsSuite) TearDownSuite() {
	s.W.Close()
}

// postsAbout are the notifications that link to a proposal.
func (s *JobsSuite) postsAbout(id uuid.UUID) []testfix.ChatPost {
	var out []testfix.ChatPost
	for _, post := range s.W.Chat.Posts() {
		if text, _ := post.Body["text"].(string); strings.Contains(text, id.String()) {
			out = append(out, post)
		}
	}
	return out
}

func (s *JobsSuite) propose(t testfix.Tenant, action string) proposals.Proposal {
	s.T().Helper()
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{IdempotencyKey: uuid.NewString(), Action: action, Title: "x", Payload: json.RawMessage(`{}`), Rationale: "Why."})
	s.Require().NoError(err)
	return p
}

// A proposal left for a person is notified once, however often it is left;
// one a mandate allows is not notified at all.
func (s *JobsSuite) Test_WhatWaitsIsNotifiedOnce() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Notifications.Set(t.Owner, notifications.KindSlack, slackUrl)
	s.Require().NoError(err)
	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.low"}, MaxRisk: mandates.RiskLow, BudgetCount: 1, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)

	manual := s.propose(t, "test.manual")
	s.Require().Eventually(func() bool { return len(s.postsAbout(manual.Id)) == 1 }, 10*time.Second, 50*time.Millisecond, "a proposal no mandate can allow is notified when it is stored")
	s.Require().Contains(s.postsAbout(manual.Id)[0].Body["text"], "a person always decides this action")

	// One at a time: two executions at once spend the budget in either order.
	allowed := s.propose(t, "test.low")
	s.Require().Eventually(func() bool {
		p, err := s.W.Kernel.Proposals.Get(t.Owner, allowed.Id)
		return err == nil && p.Status == proposals.StatusExecuted
	}, 10*time.Second, 50*time.Millisecond)
	waiting := s.propose(t, "test.low")
	s.Require().Eventually(func() bool { return len(s.postsAbout(waiting.Id)) == 1 }, 10*time.Second, 50*time.Millisecond, "the budget of one is spent by the first")
	s.Require().Contains(s.postsAbout(waiting.Id)[0].Body["text"], "budget for this month is spent")
	// Tried again, and left again: still one notification. The job is
	// queued in the transaction that leaves the proposal waiting, so once
	// ExecuteProposal returns, the queue shows whether a second one went in.
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, waiting.Id))
	s.Require().Equal(1, s.notifyJobs(waiting.Id), "a proposal left waiting again queues no second notification")
	s.Require().Len(s.postsAbout(waiting.Id), 1)
	s.Require().Empty(s.postsAbout(allowed.Id), "what a mandate allowed is nobody's to look at")

	// A tenant without a notifier queues nothing.
	quiet := s.W.NewTenant(s.T(), "Quiet")
	p, _, err := s.W.Kernel.Proposals.Propose(quiet.Agent, proposals.Input{IdempotencyKey: uuid.NewString(), Action: "test.manual", Title: "x", Payload: json.RawMessage(`{}`), Rationale: "Why."})
	s.Require().NoError(err)
	s.Require().Zero(s.notifyJobs(p.Id))
}

// notifyJobs counts the notification jobs queued for a proposal, whatever
// their state.
func (s *JobsSuite) notifyJobs(id uuid.UUID) int {
	s.T().Helper()
	var jobs int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(), `SELECT count(*) FROM mandate_river.river_job WHERE kind = 'mandate.notify' AND args->>'proposalId' = $1`, id.String()).Scan(&jobs))
	return jobs
}

// A chat tool's final refusal, such as a deleted webhook, cancels the job
// after one post; a failure it may get over is retried.
func (s *JobsSuite) Test_ARefusalCancelsTheJobAndAFailureRetriesIt() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Notifications.Set(t.Owner, notifications.KindSlack, slackUrl)
	s.Require().NoError(err)
	defer s.W.Chat.Answer(http.StatusOK)

	s.W.Chat.Answer(http.StatusNotFound)
	refused := s.propose(t, "test.manual")
	s.Require().Eventually(func() bool { state, _ := s.notifyJob(refused.Id); return state == "cancelled" }, 10*time.Second, 50*time.Millisecond, "a refusal is final")
	_, attempt := s.notifyJob(refused.Id)
	s.Require().Equal(1, attempt)
	s.Require().Len(s.postsAbout(refused.Id), 1)
	s.Require().Zero(s.notified(t, refused.Id), "only a notification that arrived is recorded")

	s.W.Chat.Answer(http.StatusBadGateway)
	failed := s.propose(t, "test.manual")
	// River runs the first retry about a second later; after the second
	// failure, the next is far enough off for the job to wait as retryable.
	s.Require().Eventually(func() bool {
		var state string
		state, attempt = s.notifyJob(failed.Id)
		return state == "retryable" && attempt >= 2
	}, 10*time.Second, 50*time.Millisecond, "a failure is retried, not cancelled")
	s.Require().Len(s.postsAbout(failed.Id), attempt, "one post per attempt")
	s.Require().Zero(s.notified(t, failed.Id), "only a notification that arrived is recorded")
}

// notifyJob is the state and attempt of the one notification job for a
// proposal, or "" before it is queued.
func (s *JobsSuite) notifyJob(id uuid.UUID) (string, int) {
	s.T().Helper()
	var state string
	var attempt int
	err := s.W.Pg.Owner.QueryRow(context.Background(), `SELECT state::text, attempt FROM mandate_river.river_job WHERE kind = 'mandate.notify' AND args->>'proposalId' = $1`, id.String()).Scan(&state, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0
	}
	s.Require().NoError(err)
	return state, attempt
}

// notified counts the proposal.notified events about a proposal.
func (s *JobsSuite) notified(t testfix.Tenant, id uuid.UUID) int {
	s.T().Helper()
	var list []events.Stored
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		var err error
		list, err = events.List(t.Owner, tx, &id, 0)
		return err
	}))
	count := 0
	for _, e := range list {
		if e.Type == "proposal.notified" {
			count++
		}
	}
	return count
}
