// Package db opens connection pools and runs transactions scoped by row level
// security. Every transaction sets mandate.tenant_id, mandate.actor_kind,
// mandate.actor_id and mandate.run_id from the context, and the policies and
// triggers read them. A context without a tenant sees no tenant data at all.
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// InTx runs fn in a transaction scoped to the tenant and actor in ctx. Keep
// fn short and never make outgoing calls inside it. If fn does not return,
// because it panics or calls runtime.Goexit (as t.FailNow does), the
// transaction is rolled back first.
//
// The transaction is read committed whatever the server's or the role's
// default: the triggers count what is there after taking a lock (the
// budget, an agent's limits, the last owner), which holds only when each
// statement sees what committed before it.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			// The rollback must happen even when fn failed because ctx was
			// cancelled, so it runs without the cancellation.
			err = errors.Join(err, rollback(context.WithoutCancel(ctx), tx))
		}
	}()

	if err := applyScope(ctx, tx); err != nil {
		return err
	}
	if err := run(ctx, tx, fn); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func run(ctx context.Context, tx pgx.Tx, fn func(tx pgx.Tx) error) error {
	returned := false
	defer func() {
		if !returned {
			_ = rollback(context.WithoutCancel(ctx), tx)
		}
	}()
	err := fn(tx)
	returned = true
	return err
}

func applyScope(ctx context.Context, tx pgx.Tx) error {
	tenantId := ""
	if id, ok := actor.TenantFrom(ctx); ok {
		tenantId = id.String()
	}
	kind, actorId := "", ""
	if a, ok := actor.From(ctx); ok {
		kind = string(a.Kind)
		if a.Id != uuid.Nil {
			actorId = a.Id.String()
		}
	}
	_, err := tx.Exec(ctx, `SELECT
		set_config('mandate.tenant_id', $1, true),
		set_config('mandate.actor_kind', $2, true),
		set_config('mandate.actor_id', $3, true),
		set_config('mandate.run_id', $4, true)`,
		tenantId, kind, actorId, actor.RunFrom(ctx))
	if err != nil {
		return fmt.Errorf("apply scope: %w", err)
	}
	return nil
}

// RequireOwner refuses the transaction's actor unless it is an owner of
// the tenant in scope, with the same answer the triggers give, so that a
// refusal does not depend on a row being there to update.
func RequireOwner(ctx context.Context, tx pgx.Tx) error {
	var role *string
	if err := tx.QueryRow(ctx, `SELECT mandate.current_role()`).Scan(&role); err != nil {
		return err
	}
	if role == nil || *role != "owner" {
		return fault.New(fault.PermissionDenied, "only an owner may do that")
	}
	return nil
}

func rollback(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("rollback: %w", err)
	}
	return nil
}
