// Package jobs wraps River, the Postgres backed job queue. Jobs are inserted
// in the same transaction as the change that causes them, so a job exists if
// and only if its cause was committed. There is no separate outbox.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/andreas-lindfalk/pgmandate/pkg/db"
)

type Queue struct {
	pool    *pgxpool.Pool
	workers *river.Workers
	client  *river.Client[pgx.Tx]
}

// NewQueue creates a queue. Register workers with Workers() before Init.
func NewQueue(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool, workers: river.NewWorkers()}
}

func (q *Queue) Workers() *river.Workers {
	return q.workers
}

// Init creates the River client once all workers are registered.
func (q *Queue) Init(maxWorkers int) error {
	if maxWorkers < 1 {
		maxWorkers = 2
	}
	client, err := river.NewClient(riverpgxv5.New(q.pool), &river.Config{
		// Deliveries call other systems and can be slow; they have their
		// own workers, so a slow connector never holds up the decisions
		// on the default queue.
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: maxWorkers},
			QueueDeliveries:    {MaxWorkers: maxWorkers},
		},
		Workers:     q.workers,
		MaxAttempts: 3,
		JobTimeout:  5 * time.Minute,
		Logger:      slog.Default(),
		// The app role does not own River's tables, so it cannot reindex
		// them. Index maintenance is the owner's job, outside the app.
		ReindexerIndexNames: []string{},
	})
	if err != nil {
		return err
	}
	q.client = client
	return nil
}

func (q *Queue) Start(ctx context.Context) error {
	if q.client == nil {
		return errors.New("jobs: Init not called")
	}
	return q.client.Start(ctx)
}

func (q *Queue) Stop(ctx context.Context) error {
	if q.client == nil {
		return nil
	}
	return q.client.Stop(ctx)
}

// InsertTx enqueues a job in the caller's transaction.
func (q *Queue) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) error {
	if q.client == nil {
		return errors.New("jobs: Init not called")
	}
	_, err := q.client.InsertTx(ctx, tx, args, opts)
	return err
}

// InFlight are the states in which a job is waiting or running, for
// uniqueness: the same work is queued once until it has run.
// QueueDeliveries is the queue for calls to connectors.
const QueueDeliveries = "deliveries"

var InFlight = []rivertype.JobState{
	rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
	rivertype.JobStateRunning, rivertype.JobStateScheduled,
}

// Migrate applies River's migrations as the owner and grants the app role
// what River needs at runtime.
func Migrate(ctx context.Context, ownerUrl string) error {
	pool, err := db.Open(ctx, ownerUrl)
	if err != nil {
		return err
	}
	defer pool.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	_, err = pool.Exec(ctx, fmt.Sprintf(`DO $$
		DECLARE r record;
		BEGIN
			FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename LIKE 'river\_%%' LOOP
				EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %%I TO %s', r.tablename);
			END LOOP;
			FOR r IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' AND sequencename LIKE 'river\_%%' LOOP
				EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE %%I TO %s', r.sequencename);
			END LOOP;
		END $$`, db.AppRole, db.AppRole))
	return err
}
