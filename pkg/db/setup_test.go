package db_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
	"github.com/andreas-lindfalk/pgmandate/pkg/testkit"
	"github.com/andreas-lindfalk/pgmandate/schema"
)

// schemas are pgmandate's: the kernel's and the job queue's.
var schemas = []string{db.Schema, jobs.Schema}

// managed is a database set up as on a managed Postgres: its admin creates
// roles and owns the database, and is no superuser. The superuser is
// there only to plant what a careless DBA might have made.
type managed struct {
	superuser string // on the managed database
	admin     string
}

func startManaged(t *testing.T) managed {
	t.Helper()
	ctx := context.Background()
	pg, superuser, err := testkit.StartBarePostgres(ctx)
	require.NoError(t, err)
	t.Cleanup(pg.Close)
	exec(t, superuser,
		`CREATE ROLE cloud_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER PASSWORD 'admin'`,
		`CREATE ROLE rds_superuser NOLOGIN`,
		`GRANT rds_superuser TO cloud_admin`,
		`CREATE DATABASE managed OWNER cloud_admin`)
	return managed{superuser: withDatabase(t, superuser, "", "", "managed"), admin: withDatabase(t, superuser, "cloud_admin", "admin", "managed")}
}

func withDatabase(t *testing.T, raw, user, password, database string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	if user != "" {
		parsed.User = url.UserPassword(user, password)
	}
	parsed.Path = "/" + database
	return parsed.String()
}

func exec(t *testing.T, dsn string, statements ...string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	require.NoError(t, err)
	defer conn.Close(context.Background()) //nolint:errcheck
	for _, statement := range statements {
		_, err := conn.Exec(context.Background(), statement)
		require.NoError(t, err, statement)
	}
}

// dropApp removes the app role, so the next case starts without one.
func (m managed) dropApp(t *testing.T) {
	t.Helper()
	exec(t, m.superuser, `DO $$ BEGIN
		IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mandate_app') THEN
			EXECUTE 'DROP OWNED BY mandate_app CASCADE';
			EXECUTE 'DROP ROLE mandate_app';
		END IF;
	END $$`)
}

func (m managed) setUp(t *testing.T, setup db.Setup) error {
	t.Helper()
	ctx := context.Background()
	if err := db.Bootstrap(ctx, m.admin, setup); err != nil {
		return err
	}
	require.NoError(t, db.Migrate(m.admin, schema.Migrations))
	require.NoError(t, jobs.Migrate(ctx, m.admin))
	pool, err := db.Open(ctx, m.admin)
	require.NoError(t, err)
	defer pool.Close()
	return db.CheckAppRole(ctx, pool, db.AppRole, schemas)
}

// An admin that is no superuser sets everything up, and the app role it
// makes can log in, is safe, and says so of itself.
func TestSetupWithoutASuperuser(t *testing.T) {
	m := startManaged(t)
	ctx := context.Background()
	require.NoError(t, m.setUp(t, db.Setup{CreateRole: true, NewRolePassword: "secret", Schemas: schemas}))
	require.NoError(t, m.setUp(t, db.Setup{CreateRole: true, NewRolePassword: "secret", Schemas: schemas}), "again, as at every start")

	app, err := db.Open(ctx, withDatabase(t, m.admin, db.AppRole, "secret", "managed"))
	require.NoError(t, err)
	defer app.Close()
	require.NoError(t, db.CheckAppRole(ctx, app, "", schemas), "the app checks itself")
	var super, bypass, createRole, inherit bool
	require.NoError(t, app.QueryRow(ctx, `SELECT rolsuper, rolbypassrls, rolcreaterole, rolinherit FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass, &createRole, &inherit))
	require.False(t, super || bypass || createRole || inherit)
	var tenants int
	require.NoError(t, app.QueryRow(ctx, `SELECT count(*) FROM mandate.tenants`).Scan(&tenants), "the app reads through row level security")
	var owner string
	require.NoError(t, app.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'mandate.tenants'::regclass`).Scan(&owner))
	require.Equal(t, "cloud_admin", owner, "the admin owns the tables, never the app")
}

// A role a DBA made beforehand is used as it is: setup neither creates it
// nor changes its password unless one is given.
func TestARoleMadeBeforehand(t *testing.T) {
	m := startManaged(t)
	ctx := context.Background()
	err := db.Bootstrap(ctx, m.admin, db.Setup{Schemas: schemas})
	require.ErrorContains(t, err, "does not exist", "without the role and without leave to create it")
	exec(t, m.superuser, `CREATE ROLE mandate_app LOGIN NOINHERIT PASSWORD 'by-the-dba'`)
	require.NoError(t, m.setUp(t, db.Setup{Schemas: schemas}))
	// Leave to create a role, but none needed and no password given: the
	// role keeps its own.
	require.NoError(t, m.setUp(t, db.Setup{CreateRole: true, NewRolePassword: "app", Schemas: schemas}))
	app, err := db.Open(ctx, withDatabase(t, m.admin, db.AppRole, "by-the-dba", "managed"))
	require.NoError(t, err, "the DBA's password still works")
	app.Close()
	// From Postgres 16 on, the admin may not set the password of a role it
	// did not make, and is told what to do instead.
	err = db.Bootstrap(ctx, m.admin, db.Setup{Password: "mine", Schemas: schemas})
	require.ErrorContains(t, err, "have whoever made the role set it")
}

// Setup refuses an app role that could get past the rules, however it got
// there, and names why.
func TestATooStrongAppRoleIsRefused(t *testing.T) {
	m := startManaged(t)
	for name, c := range map[string]struct {
		plant []string
		why   string
	}{
		"a superuser":                 {[]string{`CREATE ROLE mandate_app LOGIN SUPERUSER`}, "mandate_app is a superuser"},
		"bypasses RLS":                {[]string{`CREATE ROLE mandate_app LOGIN BYPASSRLS`}, "bypasses row level security"},
		"creates roles":               {[]string{`CREATE ROLE mandate_app LOGIN CREATEROLE`}, "can create roles"},
		"cannot log in":               {[]string{`CREATE ROLE mandate_app NOLOGIN`}, "cannot log in"},
		"replicates":                  {[]string{`CREATE ROLE mandate_app LOGIN REPLICATION`}, "replicates, and so reads every change"},
		"runs programs on the server": {[]string{`CREATE ROLE mandate_app LOGIN NOINHERIT`, `GRANT pg_execute_server_program TO mandate_app`}, "as a member of pg_execute_server_program, reaches the server's files or every table"},
		"reads every table":           {[]string{`CREATE ROLE mandate_app LOGIN NOINHERIT`, `GRANT pg_read_all_data TO mandate_app`}, "as a member of pg_read_all_data, reaches the server's files or every table"},
		"turns triggers off":          {[]string{`CREATE ROLE mandate_app LOGIN NOINHERIT`, `GRANT SET ON PARAMETER session_replication_role TO mandate_app`}, "may set session_replication_role"},
		"a managed admin's member":    {[]string{`CREATE ROLE mandate_app LOGIN NOINHERIT`, `GRANT rds_superuser TO mandate_app`}, "as a member of rds_superuser, is a managed service's admin"},
		"the admin's member, through another role": {[]string{
			`CREATE ROLE mandate_app LOGIN NOINHERIT`, `CREATE ROLE helpers NOLOGIN`, `GRANT cloud_admin TO helpers`, `GRANT helpers TO mandate_app WITH INHERIT FALSE`,
		}, "as a member of cloud_admin, can create roles"},
	} {
		t.Run(name, func(t *testing.T) {
			m.dropApp(t)
			exec(t, m.superuser, `DROP ROLE IF EXISTS helpers`)
			exec(t, m.superuser, c.plant...)
			err := db.Bootstrap(context.Background(), m.admin, db.Setup{Schemas: schemas})
			require.ErrorIs(t, err, db.ErrAppRole)
			require.ErrorContains(t, err, c.why)
		})
	}

	// After the migrations: an app role that owns what it should only use,
	// or can act as the role that does.
	m.dropApp(t)
	require.NoError(t, m.setUp(t, db.Setup{CreateRole: true, NewRolePassword: "secret", Schemas: schemas}))
	exec(t, m.superuser, `ALTER TABLE mandate.events OWNER TO mandate_app`)
	err := m.setUp(t, db.Setup{CreateRole: true, NewRolePassword: "secret", Schemas: schemas})
	require.ErrorIs(t, err, db.ErrAppRole)
	require.ErrorContains(t, err, "owns pgmandate's schemas or objects")
	exec(t, m.superuser, `ALTER TABLE mandate.events OWNER TO cloud_admin`, `CREATE ROLE owners NOLOGIN`, `GRANT owners TO mandate_app`, `ALTER TABLE mandate.events OWNER TO owners`)
	err = m.setUp(t, db.Setup{CreateRole: true, NewRolePassword: "secret", Schemas: schemas})
	require.ErrorContains(t, err, "as a member of owners, owns pgmandate's schemas or objects")
	exec(t, m.superuser, `ALTER TABLE mandate.events OWNER TO cloud_admin`, `REVOKE owners FROM mandate_app`, `DROP ROLE owners`)
	require.NoError(t, m.setUp(t, db.Setup{CreateRole: true, NewRolePassword: "secret", Schemas: schemas}))
}

// CREATE on the schema public is revoked from everyone only when asked.
func TestPublicCreateIsRevokedOnlyWhenAsked(t *testing.T) {
	m := startManaged(t)
	exec(t, m.superuser, `GRANT CREATE ON SCHEMA public TO PUBLIC`)
	mayCreate := func() bool {
		conn, err := pgx.Connect(context.Background(), m.admin)
		require.NoError(t, err)
		defer conn.Close(context.Background()) //nolint:errcheck
		var may bool
		require.NoError(t, conn.QueryRow(context.Background(), `SELECT has_schema_privilege('mandate_app', 'public', 'CREATE')`).Scan(&may))
		return may
	}
	require.NoError(t, m.setUp(t, db.Setup{CreateRole: true, NewRolePassword: "secret", Schemas: schemas}))
	require.True(t, mayCreate(), "left alone unless asked, with a warning")
	conn, err := pgx.Connect(context.Background(), m.admin)
	require.NoError(t, err)
	may, err := db.MayCreateInPublic(context.Background(), conn, db.AppRole)
	require.NoError(t, err)
	require.True(t, may)
	require.NoError(t, conn.Close(context.Background()))
	require.NoError(t, m.setUp(t, db.Setup{CreateRole: true, NewRolePassword: "secret", RevokePublicCreate: true, Schemas: schemas}))
	require.False(t, mayCreate())
}
