package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the pgx database/sql driver
)

// Schema is the kernel's schema.
const Schema = "mandate"

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

// Setup says how Bootstrap treats the application role and the public
// schema. Its zero value creates nothing and changes nothing: a role a DBA
// made beforehand is only checked.
type Setup struct {
	// CreateRole creates the app role when it is missing, with LOGIN and
	// none of the attributes that get past row level security, and the
	// password NewRolePassword.
	CreateRole      bool
	NewRolePassword string
	// Password, when set, is given to the app role even when it was already
	// there. Empty leaves an existing role's password as it is.
	Password string
	// RevokePublicCreate revokes CREATE on the schema public from PUBLIC.
	// Postgres 15 and later grant none by default; older ones and some
	// managed services do, which lets any role, the app's among them, put
	// objects there.
	RevokePublicCreate bool
	// Schemas are pgmandate's schemas, which the app role must not own: the
	// kernel's and the job queue's.
	Schemas []string
}

// Bootstrap prepares the application role as the owner, before the
// migrations, and is safe to run repeatedly. It never sets a role
// attribute other than LOGIN and the password: changing SUPERUSER or
// BYPASSRLS needs a superuser, which a managed Postgres's admin is not.
// Instead it checks the role (CheckAppRole) and refuses to go on when it
// is wrong.
func Bootstrap(ctx context.Context, ownerUrl string, setup Setup) error {
	conn, err := pgx.Connect(ctx, ownerUrl)
	if err != nil {
		return fmt.Errorf("connect as owner: %w", err)
	}
	defer conn.Close(ctx) //nolint:errcheck

	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, AppRole).Scan(&exists); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	password := setup.Password
	switch {
	case !exists && !setup.CreateRole:
		return fmt.Errorf("bootstrap: the role %s does not exist; create it, or let setup create it", AppRole)
	case !exists:
		if password == "" {
			password = setup.NewRolePassword
		}
		if password == "" {
			return errors.New("bootstrap: an app password is required to create the app role")
		}
		// Only the attributes a non-superuser with CREATEROLE may give.
		if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %s`,
			AppRole, quoteLiteral(password))); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	case password != "":
		if _, err := conn.Exec(ctx, fmt.Sprintf(`ALTER ROLE %s WITH LOGIN PASSWORD %s`, AppRole, quoteLiteral(password))); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				// From Postgres 16 on, only a role with ADMIN on it may,
				// which an admin does not have for a role a DBA made.
				return fmt.Errorf("bootstrap: setting the password of %s was refused (%s); have whoever made the role set it, and leave the app password unset here", AppRole, pgErr.Message)
			}
			return fmt.Errorf("bootstrap: %w", err)
		}
	}
	if setup.RevokePublicCreate {
		if _, err := conn.Exec(ctx, `REVOKE CREATE ON SCHEMA public FROM PUBLIC`); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, AppRole)); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if may, err := MayCreateInPublic(ctx, conn, AppRole); err != nil {
		return err
	} else if may {
		slog.WarnContext(ctx, "the app role may create objects in the schema public, where the owner's migrations look for functions; revoke it, as MANDATE_REVOKE_PUBLIC_CREATE=true does", "role", AppRole)
	}
	return CheckAppRole(ctx, conn, AppRole, setup.Schemas)
}

// MayCreateInPublic says whether the role may create objects in the schema
// public, as through a grant to PUBLIC.
func MayCreateInPublic(ctx context.Context, q Querier, role string) (bool, error) {
	var may bool
	if err := q.QueryRow(ctx, `SELECT has_schema_privilege($1, 'public', 'CREATE')`, role).Scan(&may); err != nil {
		return false, fmt.Errorf("check the app role: %w", err)
	}
	return may, nil
}

// Querier is what CheckAppRole reads with: a connection, a pool or a
// transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// managedAdmins are the admin roles of managed Postgres services. None of
// them is a superuser, but each can do what row level security and the
// append-only guard are there to stop, so the app role must not be able to
// act as one.
var managedAdmins = []string{
	"rds_superuser", "rdsadmin", // Amazon RDS and Aurora
	"cloudsqlsuperuser", "cloudsqladmin", // Google Cloud SQL
	"alloydbsuperuser", "alloydbadmin", // Google AlloyDB
	"azure_pg_admin", "azuresu", // Azure Database for PostgreSQL
	"supabase_admin", // Supabase
	"neon_superuser", // Neon
}

// powerfulPredefined are Postgres's own roles that get past the rules: a
// shell or the data files on the server, or every table past the grants.
var powerfulPredefined = []string{
	"pg_execute_server_program", "pg_read_server_files", "pg_write_server_files",
	"pg_read_all_data", "pg_write_all_data",
}

// ErrAppRole is returned when the app role could get past the rules.
var ErrAppRole = errors.New("the app role is not safe to serve with")

// CheckAppRole refuses an app role that could get past row level security
// or the append-only guard: one that cannot log in, may set
// session_replication_role (which turns triggers off), or is, or can act as
// through membership (with or without INHERIT, since SET ROLE is enough), a
// role that is a superuser, bypasses row level security, creates roles,
// replicates (and so reads every change), is a managed service's admin or
// one of Postgres's own roles with the server's files or every table, owns
// any of schemas or anything in them, or may truncate or add triggers to a
// table in them (TRUNCATE fires no row triggers and ignores row level
// security, so it empties a table for every tenant). role is a role name,
// or empty for the current user, as when the app checks itself at start.
func CheckAppRole(ctx context.Context, q Querier, role string, schemas []string) error {
	if role == "" {
		if err := q.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
			return fmt.Errorf("check the app role: %w", err)
		}
	}
	var canLogin bool
	err := q.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, role).Scan(&canLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the role %s does not exist", ErrAppRole, role)
	}
	if err != nil {
		return fmt.Errorf("check the app role: %w", err)
	}
	var problems []string
	if !canLogin {
		problems = append(problems, role+" cannot log in")
	}
	// Granting SET on a parameter exists from Postgres 15 on; before, only
	// a superuser could set this one.
	var setsReplicationRole bool
	if err := q.QueryRow(ctx, `SELECT current_setting('server_version_num')::int >= 150000
		AND has_parameter_privilege($1, 'session_replication_role', 'SET')`, role).Scan(&setsReplicationRole); err != nil {
		return fmt.Errorf("check the app role: %w", err)
	}
	if setsReplicationRole {
		problems = append(problems, role+" may set session_replication_role, which turns the triggers off")
	}
	// Every role the app role is, or can become: itself and every role it
	// is a member of, directly or through others.
	rows, err := q.Query(ctx, `
		SELECT r.rolname, r.rolsuper, r.rolbypassrls, r.rolcreaterole, r.rolreplication,
			r.rolname = ANY($2::text[]), r.rolname = ANY($3::text[]),
			EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname = ANY($4::text[]) AND n.nspowner = r.oid)
			OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = ANY($4::text[]) AND c.relowner = r.oid)
			OR EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
				WHERE n.nspname = ANY($4::text[]) AND p.proowner = r.oid),
			EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = ANY($4::text[]) AND c.relkind IN ('r', 'p')
				AND (has_table_privilege(r.oid, c.oid, 'TRUNCATE') OR has_table_privilege(r.oid, c.oid, 'TRIGGER')))
		FROM pg_roles r
		WHERE pg_has_role($1, r.oid, 'MEMBER')
		ORDER BY r.rolname`, role, managedAdmins, powerfulPredefined, schemas)
	if err != nil {
		return fmt.Errorf("check the app role: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var super, bypassRls, createRole, replication, managedAdmin, predefined, owner, truncates bool
		if err := rows.Scan(&name, &super, &bypassRls, &createRole, &replication, &managedAdmin, &predefined, &owner, &truncates); err != nil {
			return fmt.Errorf("check the app role: %w", err)
		}
		who := role
		if name != role {
			who = role + ", as a member of " + name + ","
		}
		if super {
			problems = append(problems, who+" is a superuser")
		}
		if bypassRls {
			problems = append(problems, who+" bypasses row level security")
		}
		if createRole {
			problems = append(problems, who+" can create roles")
		}
		if replication {
			problems = append(problems, who+" replicates, and so reads every change")
		}
		if managedAdmin {
			problems = append(problems, who+" is a managed service's admin")
		}
		if predefined {
			problems = append(problems, who+" reaches the server's files or every table")
		}
		if owner {
			problems = append(problems, who+" owns pgmandate's schemas or objects")
		}
		if truncates {
			problems = append(problems, who+" may truncate or add triggers to pgmandate's tables, which gets past row level security and the append-only guard")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check the app role: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s. Give the app its own role that is none of these, and is a member of no role that is", ErrAppRole, strings.Join(problems, "; "))
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
