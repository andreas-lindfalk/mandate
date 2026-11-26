package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
	"github.com/andreas-lindfalk/pgmandate/pkg/testkit"
)

type probeArgs struct{}

func (probeArgs) Kind() string { return "test.probe" }

// probeWorker reports each run of a job on ran.
type probeWorker struct {
	river.WorkerDefaults[probeArgs]
	ran chan int64
}

func (w *probeWorker) Work(_ context.Context, job *river.Job[probeArgs]) error {
	w.ran <- job.ID
	return nil
}

// RescueSuite shows that a job left running by a process that died is run
// again soon after the longest a job can run, not River's default of
// JobTimeout + 1h. Every job is unique while it runs, so until it is
// rescued nothing else can take its place.
type RescueSuite struct {
	suite.Suite
	pg *testkit.Postgres
}

func TestRescueSuite(t *testing.T) {
	suite.Run(t, new(RescueSuite))
}

func (s *RescueSuite) SetupSuite() {
	pg, err := testkit.StartPostgres(context.Background())
	s.Require().NoError(err)
	s.pg = pg
}

func (s *RescueSuite) TearDownSuite() {
	if s.pg != nil {
		s.pg.Close()
	}
}

func (s *RescueSuite) Test_JobOfADeadProcessRunsAgain() {
	ctx := context.Background()
	queue := jobs.NewQueue(s.pg.App)
	worker := &probeWorker{ran: make(chan int64, 4)}
	river.AddWorker(queue.Workers(), worker)
	s.Require().NoError(queue.Init(1))
	s.Require().NoError(queue.Insert(ctx, probeArgs{}, &river.InsertOpts{
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: jobs.InFlight},
	}))
	var id int64
	s.Require().NoError(s.pg.Owner.QueryRow(ctx,
		`SELECT id FROM mandate_river.river_job WHERE kind = 'test.probe'`).Scan(&id))
	defer func() {
		_, err := s.pg.Owner.Exec(ctx, `DELETE FROM mandate_river.river_job WHERE id = $1`, id)
		s.Require().NoError(err)
	}()

	// The process running the job died: the job has now been running two
	// minutes longer than any job can, well short of River's default
	// JobTimeout + 1h.
	attemptedAt := time.Now().Add(-(jobs.JobTimeout + 2*time.Minute))
	_, err := s.pg.Owner.Exec(ctx, `
		UPDATE mandate_river.river_job
		SET state = 'running', attempt = 1, attempted_at = $2, attempted_by = ARRAY['dead-client']
		WHERE id = $1`, id, attemptedAt)
	s.Require().NoError(err)

	// While it is running, the same work cannot be queued in its place.
	s.Require().NoError(queue.Insert(ctx, probeArgs{}, &river.InsertOpts{
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: jobs.InFlight},
	}))
	var count int
	s.Require().NoError(s.pg.Owner.QueryRow(ctx,
		`SELECT count(*) FROM mandate_river.river_job WHERE kind = 'test.probe'`).Scan(&count))
	s.Require().Equal(1, count)

	s.Require().NoError(queue.Start(ctx))
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		s.Require().NoError(queue.Stop(stopCtx))
	}()

	select {
	case ran := <-worker.ran:
		s.Require().Equal(id, ran)
	case <-time.After(90 * time.Second):
		s.Require().Fail("the job of a dead process was not run again")
	}

	var state rivertype.JobState
	var attempt int
	// Eventually runs the condition on its own goroutine, where Require
	// cannot stop the test, so a failed read just tries again.
	s.Require().Eventually(func() bool {
		err := s.pg.Owner.QueryRow(ctx,
			`SELECT state, attempt FROM mandate_river.river_job WHERE id = $1`, id).Scan(&state, &attempt)
		return err == nil && state == rivertype.JobStateCompleted
	}, 10*time.Second, 100*time.Millisecond)
	s.Require().Equal(2, attempt)
}
