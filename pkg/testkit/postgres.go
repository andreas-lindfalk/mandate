// Package testkit starts a real Postgres for integration tests with
// testcontainers, with the same roles and migrations as production. Tests
// never run against mocks of the database: the database is the point.
package testkit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
	"github.com/andreas-lindfalk/pgmandate/schema"
)

const (
	PostgresImage = "postgres:18-alpine"
	ownerUser     = "mandate_owner"
	ownerPassword = "owner"
	appPassword   = "app"
	databaseName  = "mandate"
)

type Postgres struct {
	Container *postgres.PostgresContainer
	OwnerUrl  string
	AppUrl    string
	// Owner bypasses row level security. Use it only for test setup, cleanup
	// and assertions that must see every tenant.
	Owner *pgxpool.Pool
	// App connects as mandate_app and is subject to row level security, like
	// the running application.
	App *pgxpool.Pool
}

// StartPostgres starts Postgres, bootstraps the roles and applies mandate's
// migrations, River's, and then the given sets (a host's own schema).
func StartPostgres(ctx context.Context, sets ...db.Migrations) (*Postgres, error) {
	container, err := postgres.Run(ctx, PostgresImage,
		postgres.WithDatabase(databaseName),
		postgres.WithUsername(ownerUser),
		postgres.WithPassword(ownerPassword),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	pg := &Postgres{Container: container}

	pg.OwnerUrl, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, pg.fail(err)
	}
	pg.AppUrl, err = db.AppUrl(pg.OwnerUrl, appPassword)
	if err != nil {
		return nil, pg.fail(err)
	}
	if err := db.Bootstrap(ctx, pg.OwnerUrl, db.Setup{CreateRole: true, NewRolePassword: appPassword, Password: appPassword, Schemas: []string{db.Schema, jobs.Schema}}); err != nil {
		return nil, pg.fail(err)
	}
	if err := db.Migrate(pg.OwnerUrl, append([]db.Migrations{schema.Migrations}, sets...)...); err != nil {
		return nil, pg.fail(err)
	}
	if err := jobs.Migrate(ctx, pg.OwnerUrl); err != nil {
		return nil, pg.fail(err)
	}
	if pg.Owner, err = db.Open(ctx, pg.OwnerUrl); err != nil {
		return nil, pg.fail(err)
	}
	if pg.App, err = db.Open(ctx, pg.AppUrl); err != nil {
		return nil, pg.fail(err)
	}
	return pg, nil
}

// StartBarePostgres starts Postgres with nothing set up, and returns it with
// a superuser's connection, for tests of the setup itself.
func StartBarePostgres(ctx context.Context) (*Postgres, string, error) {
	container, err := postgres.Run(ctx, PostgresImage,
		postgres.WithDatabase(databaseName),
		postgres.WithUsername(ownerUser),
		postgres.WithPassword(ownerPassword),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, "", fmt.Errorf("start postgres: %w", err)
	}
	pg := &Postgres{Container: container}
	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, "", pg.fail(err)
	}
	return pg, url, nil
}

func (p *Postgres) Close() {
	if p.App != nil {
		p.App.Close()
	}
	if p.Owner != nil {
		p.Owner.Close()
	}
	if p.Container != nil {
		_ = testcontainers.TerminateContainer(p.Container)
	}
}

func (p *Postgres) fail(err error) error {
	p.Close()
	return err
}

// PurgeTenant deletes a tenant and everything under it, including
// append-only rows, the way an erasure does in production.
func (p *Postgres) PurgeTenant(ctx context.Context, tenantId any) error {
	_, err := p.Owner.Exec(ctx, `SELECT mandate.purge_tenant($1)`, tenantId)
	return err
}
