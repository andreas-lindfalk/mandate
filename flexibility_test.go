package pgmandate_test

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate"
	agentv1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/agent/v1"
	"github.com/andreas-lindfalk/pgmandate/modules/agentapi"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// FlexibilitySuite holds what the scope says stays flexible: a host that
// embeds pgmandate and runs River itself, and several server instances on
// one database. It runs with the world's queue, and starts a second
// instance beside it.
type FlexibilitySuite struct {
	IntegrationSuite
	second *pgmandate.Kernel
	stop   func()
}

func TestFlexibilitySuite(t *testing.T) {
	suite.Run(t, &FlexibilitySuite{IntegrationSuite: IntegrationSuite{WithJobs: true}})
}

func (s *FlexibilitySuite) SetupSuite() {
	s.IntegrationSuite.SetupSuite()
	second, stop, err := s.W.Instance(context.Background())
	s.Require().NoError(err)
	s.second, s.stop = second, stop
}

func (s *FlexibilitySuite) TearDownSuite() {
	if s.stop != nil {
		s.stop()
	}
	s.IntegrationSuite.TearDownSuite()
}

// hostNote is a job of the host's own, on its own River.
type hostNote struct {
	Text string `json:"text"`
}

func (hostNote) Kind() string { return "host.note" }

// A host that embeds pgmandate runs River itself, in its own schema
// (public, River's default) and with its own role, no superuser, in the
// same binary and database. The two queues each do their own work and
// never see each other's, and pgmandate's role has nothing of the host's.
func (s *FlexibilitySuite) Test_AnEmbeddedHostRunsItsOwnRiver() {
	ctx := context.Background()
	// The host's own role, which may create in public and nothing more.
	for _, statement := range []string{
		`CREATE ROLE host_app LOGIN PASSWORD 'host' NOSUPERUSER`,
		`GRANT CREATE, USAGE ON SCHEMA public TO host_app`,
	} {
		_, err := s.W.Pg.Owner.Exec(ctx, statement)
		s.Require().NoError(err)
	}
	hostUrl, err := url.Parse(s.W.Pg.OwnerUrl)
	s.Require().NoError(err)
	hostUrl.User = url.UserPassword("host_app", "host")
	hostPool, err := db.Open(ctx, hostUrl.String())
	s.Require().NoError(err)
	defer hostPool.Close()

	host := riverpgxv5.New(hostPool)
	migrator, err := rivermigrate.New(host, nil)
	s.Require().NoError(err)
	_, err = migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	s.Require().NoError(err, "the host migrates its own River, without a superuser")
	notes := make(chan string, 1)
	workers := river.NewWorkers()
	river.AddWorker(workers, river.WorkFunc(func(_ context.Context, job *river.Job[hostNote]) error {
		notes <- job.Args.Text
		return nil
	}))
	client, err := river.NewClient(host, &river.Config{Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}}, Workers: workers})
	s.Require().NoError(err)
	s.Require().NoError(client.Start(ctx))
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	_, err = client.Insert(ctx, hostNote{Text: "the host's own work"}, nil)
	s.Require().NoError(err)
	select {
	case text := <-notes:
		s.Require().Equal("the host's own work", text)
	case <-time.After(10 * time.Second):
		s.Require().FailNow("the host's River never ran its job")
	}

	// pgmandate's queue works beside it: a proposal a mandate allows is
	// executed by pgmandate's own job.
	t := s.W.NewTenant(s.T(), "Acme")
	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"test.low"}, MaxRisk: mandates.RiskLow, BudgetCount: 5, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "test.low", Title: "T", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	s.Require().Eventually(func() bool {
		got, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
		return err == nil && got.Status == proposals.StatusExecuted
	}, 15*time.Second, 50*time.Millisecond)

	// Apart: neither table holds the other's jobs, and neither role can
	// reach the other's queue.
	var hostHasOurs, oursHasHost int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM public.river_job WHERE kind LIKE 'mandate.%'),
		(SELECT count(*) FROM mandate_river.river_job WHERE kind = 'host.note')`).Scan(&hostHasOurs, &oursHasHost))
	s.Require().Zero(hostHasOurs)
	s.Require().Zero(oursHasHost)
	err = db.InTx(t.System, s.W.Pg.App, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.System, `SELECT count(*) FROM public.river_job`)
		return err
	})
	s.Require().Error(err)
	s.Require().Equal(fault.Name(fault.PermissionDenied), fault.Name(fault.CodeOf(fault.FromDb(err))))
	_, err = hostPool.Exec(ctx, `SELECT count(*) FROM mandate_river.river_job`)
	s.Require().Error(err, "the host's role has nothing of pgmandate's queue either")
}

// Two instances on one database hold the budget together: proposals that
// both execute at the same moment spend exactly what the mandate allows,
// and each is applied once, by whichever instance won.
func (s *FlexibilitySuite) Test_TwoInstancesShareTheBudget() {
	t := s.W.NewTenant(s.T(), "Acme")
	// Made before any mandate covers them: each one's job runs and leaves
	// it waiting, so the race below is the instances', not the queues'.
	var made []proposals.Proposal
	for range 8 {
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "test.low", Title: "T", Payload: json.RawMessage(`{}`)})
		s.Require().NoError(err)
		made = append(made, p)
	}
	s.Require().Eventually(func() bool {
		for _, p := range made {
			reason, found, err := s.W.Kernel.Proposals.SkipReason(t.Owner, p.Id)
			if err != nil || !found || reason != mandates.SkipNoMandate {
				return false
			}
		}
		return true
	}, 15*time.Second, 50*time.Millisecond, "every queued execution has run and found no mandate")
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.low"}, MaxRisk: mandates.RiskLow, BudgetCount: 3, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)

	// Both instances execute every proposal, all at once.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	start := make(chan struct{})
	for _, p := range made {
		for _, k := range []*pgmandate.Kernel{s.W.Kernel, s.second} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := k.ExecuteProposal(context.Background(), t.Id, p.Id); err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	s.Require().Empty(failures, "losing a race is no failure of the job")

	spent, err := s.W.Kernel.Mandates.Spent(t.Owner, m.Id)
	s.Require().NoError(err)
	s.Require().Equal(int64(3), spent, "the budget holds across instances")
	applied := map[uuid.UUID]int{}
	for _, id := range s.W.Applied() {
		applied[id]++
	}
	executed := 0
	for _, p := range made {
		got, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
		s.Require().NoError(err)
		if got.Status == proposals.StatusExecuted {
			executed++
			s.Require().Equal(1, applied[p.Id], "applied once, whichever instance ran it")
		} else {
			s.Require().Zero(applied[p.Id])
		}
	}
	s.Require().Equal(3, executed)
}

// One decision per proposal across instances, the per-agent proposal
// limit across instances, and one chain however many instances write it.
func (s *FlexibilitySuite) Test_TwoInstancesShareDecisionsLimitsAndTheLog() {
	t := s.W.NewTenant(s.T(), "Acme")

	// A person approves on one instance while another rejects on the other:
	// one wins, the other is told it is decided already.
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "test.manual", Title: "T", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	var wg sync.WaitGroup
	var approveErr, rejectErr error
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, approveErr = s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, rejectErr = s.second.Proposals.Reject(t.Reviewer, p.Id, "No.")
	}()
	close(start)
	wg.Wait()
	s.Require().True((approveErr == nil) != (rejectErr == nil), "one decision: %v, %v", approveErr, rejectErr)
	loser, want := rejectErr, proposals.StatusApproved
	if approveErr != nil {
		loser, want = approveErr, proposals.StatusRejected
	}
	s.Require().Equal(fault.Name(fault.FailedPrecondition), fault.Name(fault.CodeOf(loser)), "the loser lost the race, not for another reason: %v", loser)
	got, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(want, got.Status)
	decisions := 0
	for _, e := range s.eventTypes(t.Owner, p.Id) {
		if e == "proposal.approved" || e == "proposal.rejected" {
			decisions++
		}
	}
	s.Require().Equal(1, decisions)

	// The agent's limit per minute holds across both.
	_, err = s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, 4, 100)
	s.Require().NoError(err)
	results := make([]error, 10)
	for i := range results {
		k := s.W.Kernel
		if i%2 == 1 {
			k = s.second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, results[i] = k.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "test.manual", Title: "T", Payload: json.RawMessage(`{}`)})
		}()
	}
	wg.Wait()
	made, limited := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			made++
		case fault.CodeOf(err) == fault.RateLimited:
			limited++
		default:
			s.Require().NoError(err)
		}
	}
	// One proposal this minute already, before the limit was set.
	s.Require().Equal(3, made, "four a minute, one of them made before")
	s.Require().Equal(7, limited)

	// One chain, written by both, verifies.
	var count int64
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		var err error
		count, err = events.Verify(t.Owner, tx)
		return err
	}))
	s.Require().Positive(count)
}

// The per-second call limit is kept in each instance's memory, as the
// threat model says: once one instance refuses a token, the other still
// allows it its own burst.
func (s *FlexibilitySuite) Test_TheCallLimitIsPerInstance() {
	t := s.W.NewTenant(s.T(), "Acme")
	ctx := agentapi.WithCall(context.Background(), agentapi.Call{Grant: t.Grant, Via: agentapi.ViaHttp})
	// drain calls until the instance refuses for the rate, and says how
	// many it allowed and in how long. Any other error fails the test.
	drain := func(k *pgmandate.Kernel) (int, time.Duration) {
		began := time.Now()
		for allowed := 0; allowed < 500; allowed++ {
			_, err := k.AgentApi.ListProposals(ctx, &agentv1.ListProposalsRequest{})
			if err == nil {
				continue
			}
			_, limited := agentapi.RetryAfterOf(err)
			s.Require().True(limited, "refused for the rate, not for %v", err)
			return allowed, time.Since(began)
		}
		s.Require().FailNow("never limited")
		return 0, 0
	}
	// A burst of 30, and five a second refilled meanwhile.
	within := func(allowed int, took time.Duration) {
		s.T().Helper()
		s.Require().GreaterOrEqual(allowed, 30)
		s.Require().LessOrEqual(float64(allowed), 30+5*took.Seconds()+1)
	}
	first, took := drain(s.W.Kernel)
	within(first, took)
	second, took := drain(s.second)
	within(second, took)
}
