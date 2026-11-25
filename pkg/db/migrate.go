package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the pgx database/sql driver
)

// AppRole is the role the application connects as. It is never a superuser,
// never owns tables and never bypasses row level security.
const AppRole = "mandate_app"

// Migrations is one migration set. Each set keeps its own migrations table,
// so a host's own schema can evolve independently of mandate's.
type Migrations struct {
	Name string
	Fs   fs.FS
	Dir  string
}

// Bootstrap creates or updates the application role. It runs as the owner,
// before migrations, and is safe to run repeatedly.
func Bootstrap(ctx context.Context, ownerUrl, appPassword string) error {
	if appPassword == "" {
		return errors.New("app password is required")
	}
	conn, err := pgx.Connect(ctx, ownerUrl)
	if err != nil {
		return fmt.Errorf("connect as owner: %w", err)
	}
	defer conn.Close(ctx) //nolint:errcheck

	statements := []string{
		fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s') THEN
				CREATE ROLE %s LOGIN;
			END IF;
		END $$`, AppRole, AppRole),
		fmt.Sprintf(`ALTER ROLE %s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD %s`,
			AppRole, quoteLiteral(appPassword)),
		`REVOKE CREATE ON SCHEMA public FROM PUBLIC`,
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, AppRole),
	}
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}
	return nil
}

// Migrate applies the migration sets in the given order, as the owner.
func Migrate(ownerUrl string, sets ...Migrations) error {
	for _, set := range sets {
		if err := migrateOne(ownerUrl, set); err != nil {
			return fmt.Errorf("migrate %s: %w", set.Name, err)
		}
	}
	return nil
}

func migrateOne(ownerUrl string, set Migrations) error {
	sqlDb, err := sql.Open("pgx", ownerUrl)
	if err != nil {
		return err
	}
	defer sqlDb.Close() //nolint:errcheck

	driver, err := pgxmigrate.WithInstance(sqlDb, &pgxmigrate.Config{
		MigrationsTable: "schema_migrations_" + set.Name,
	})
	if err != nil {
		return err
	}
	source, err := iofs.New(set.Fs, set.Dir)
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// AppUrl is the owner url with the app role's credentials: how the running
// application connects.
func AppUrl(ownerUrl, appPassword string) (string, error) {
	parsed, err := url.Parse(ownerUrl)
	if err != nil {
		return "", err
	}
	parsed.User = url.UserPassword(AppRole, appPassword)
	return parsed.String(), nil
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
