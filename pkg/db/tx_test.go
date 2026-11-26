package db_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/testkit"
)

// lockKey is the advisory lock fn takes inside the transaction, standing in
// for the row locks and the chain lock real transactions hold.
const lockKey = 42

// TxSuite shows that InTx releases its transaction however fn ends: a
// transaction left open would keep its connection and its locks, and
// everything waiting on them would block.
type TxSuite struct {
	suite.Suite
	pg *testkit.Postgres
	// pool has a single connection, so a transaction that is not released
	// shows at once, and the next transaction runs on the same backend.
	pool *pgxpool.Pool
}

func TestTxSuite(t *testing.T) {
	suite.Run(t, new(TxSuite))
}

func (s *TxSuite) SetupSuite() {
	ctx := context.Background()
	pg, err := testkit.StartPostgres(ctx)
	s.Require().NoError(err)
	s.pg = pg
	_, err = pg.Owner.Exec(ctx, `CREATE TABLE public.tx_scratch (n int NOT NULL);
		GRANT SELECT, INSERT ON public.tx_scratch TO mandate_app`)
	s.Require().NoError(err)
	config, err := pgxpool.ParseConfig(pg.AppUrl)
	s.Require().NoError(err)
	config.MaxConns = 1
	s.pool, err = pgxpool.NewWithConfig(ctx, config)
	s.Require().NoError(err)
}

func (s *TxSuite) TearDownSuite() {
	// Close waits for every connection to come back, so after a leak, which
	// a test has already reported, the container is left to end it.
	if s.pool != nil && s.pool.Stat().AcquiredConns() == 0 {
		s.pool.Close()
	}
	if s.pg != nil {
		s.pg.Close()
	}
}

// lockAndInsert takes the advisory lock and writes a row, and returns the
// backend that did it.
func (s *TxSuite) lockAndInsert(ctx context.Context, tx pgx.Tx, n int) int32 {
	var pid int32
	s.Require().NoError(tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM (SELECT pg_advisory_xact_lock($1)) AS locked`, lockKey).Scan(&pid))
	_, err := tx.Exec(ctx, `INSERT INTO public.tx_scratch (n) VALUES ($1)`, n)
	s.Require().NoError(err)
	return pid
}

// requireReleased asserts that no connection is checked out, that another
// transaction can take the lock at once, and that row n was never committed.
func (s *TxSuite) requireReleased(n int) {
	s.T().Helper()
	s.Require().Zero(s.pool.Stat().AcquiredConns(), "the transaction's connection is still checked out")
	ctx := s.ctx()
	s.Require().NoError(db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var free bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey).Scan(&free); err != nil {
			return err
		}
		s.Require().True(free, "the lock taken in the transaction is still held")
		var rows int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.tx_scratch WHERE n = $1`, n).Scan(&rows); err != nil {
			return err
		}
		s.Require().Zero(rows, "the row written in the transaction was committed")
		return nil
	}))
}

func (s *TxSuite) backendPid() int32 {
	s.T().Helper()
	var pid int32
	s.Require().NoError(s.pool.QueryRow(s.ctx(), `SELECT pg_backend_pid()`).Scan(&pid))
	return pid
}

// ctx bounds a test, so that a transaction left open makes the next one fail
// instead of waiting for the connection forever.
func (s *TxSuite) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	s.T().Cleanup(cancel)
	return ctx
}

func (s *TxSuite) Test_APanicRollsBackAndReleases() {
	ctx := s.ctx()
	recovered := func() (r any) {
		defer func() { r = recover() }()
		_ = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			s.lockAndInsert(ctx, tx, 1)
			panic("boom")
		})
		return nil
	}()
	s.Require().Equal("boom", recovered, "InTx must let the panic through")
	s.requireReleased(1)
}

func (s *TxSuite) Test_GoexitRollsBackAndReleases() {
	ctx := s.ctx()
	done := make(chan struct{})
	reachedEnd := false
	go func() {
		defer close(done)
		_ = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			s.lockAndInsert(ctx, tx, 2)
			runtime.Goexit()
			return nil
		})
		reachedEnd = true
	}()
	<-done
	s.Require().False(reachedEnd, "Goexit must end the goroutine")
	s.requireReleased(2)
}

func (s *TxSuite) Test_AnErrorRollsBackAndReleases() {
	ctx := s.ctx()
	refused := errors.New("refused")
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		s.lockAndInsert(ctx, tx, 3)
		return refused
	})
	s.Require().ErrorIs(err, refused)
	s.requireReleased(3)
}

// A cancelled context still gets its rollback, so the connection goes back
// to the pool alive instead of being closed in an undefined state.
func (s *TxSuite) Test_ACancelledContextRollsBackAndKeepsTheConnection() {
	ctx, cancel := context.WithCancel(s.ctx())
	defer cancel()
	var pid int32
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		pid = s.lockAndInsert(ctx, tx, 4)
		cancel()
		return ctx.Err()
	})
	s.Require().ErrorIs(err, context.Canceled)
	s.requireReleased(4)
	s.Require().Equal(pid, s.backendPid(), "the connection was closed instead of rolled back")
}

func (s *TxSuite) Test_ACommitIsVisible() {
	ctx := s.ctx()
	s.Require().NoError(db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		s.lockAndInsert(ctx, tx, 5)
		return nil
	}))
	s.Require().Zero(s.pool.Stat().AcquiredConns())
	var rows int
	s.Require().NoError(s.pool.QueryRow(ctx, `SELECT count(*) FROM public.tx_scratch WHERE n = 5`).Scan(&rows))
	s.Require().Equal(1, rows)
}
