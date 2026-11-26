package tenants_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/modules/tenants"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type TenantsSuite struct {
	IntegrationSuite
}

func TestTenantsSuite(t *testing.T) {
	suite.Run(t, new(TenantsSuite))
}

func (s *TenantsSuite) Test_CreateTenantWritesTheChain() {
	t := s.W.NewTenant(s.T(), "Acme")
	got, err := s.W.Kernel.Tenants.Get(t.System)
	s.Require().NoError(err)
	s.Require().Equal("Acme", got.Name)
	s.Require().Equal([]string{"tenant.created"}, s.eventTypes(t.System, t.Id))

	_, err = s.W.Kernel.Tenants.Create(context.Background(), "  ")
	s.code(err, fault.InvalidArgument)
}

func (s *TenantsSuite) Test_OnlyOwnersAddPeople() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Tenants.AddPerson(t.Reviewer, "x@test.example", "X", tenants.RoleReviewer)
	s.code(err, fault.PermissionDenied)
	_, err = s.W.Kernel.Tenants.AddPerson(t.Agent, "x@test.example", "X", tenants.RoleReviewer)
	s.code(err, fault.PermissionDenied)

	added, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "Second.Owner@Test.example", "Second", tenants.RoleOwner)
	s.Require().NoError(err)
	s.Require().Equal("second.owner@test.example", added.Email, "emails are stored lower case")
	s.Require().Equal([]string{"person.added"}, s.eventTypes(t.Owner, added.Id))

	found, err := s.W.Kernel.Tenants.FindPerson(t.System, "second.owner@test.example")
	s.Require().NoError(err)
	s.Require().Equal(added.Id, found.Id)
	people, err := s.W.Kernel.Tenants.ListPersons(t.Reviewer)
	s.Require().NoError(err)
	s.Require().Len(people, 3)

	_, err = s.W.Kernel.Tenants.AddPerson(t.Owner, "second.owner@test.example", "Again", tenants.RoleOwner)
	s.code(err, fault.Conflict)
	for _, bad := range []struct{ email, name, role string }{
		{"no-at", "X", tenants.RoleOwner}, {"x@test.example", "", tenants.RoleOwner}, {"x@test.example", "X", "admin"},
	} {
		_, err = s.W.Kernel.Tenants.AddPerson(t.Owner, bad.email, bad.name, bad.role)
		s.code(err, fault.InvalidArgument)
	}
}

func (s *TenantsSuite) Test_AnOwnerRemovesPeople() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.code(s.W.Kernel.Tenants.RemovePerson(t.Reviewer, t.OwnerId), fault.PermissionDenied)
	s.code(s.W.Kernel.Tenants.RemovePerson(t.Agent, t.ReviewerId), fault.PermissionDenied)

	// A session is by email and spans tenants: removal from this tenant
	// leaves it alone, so one tenant's owner cannot sign a person out of
	// their other tenants. Access to this tenant ends with the membership.
	reviewerEmail := "reviewer-" + t.Id.String()[:8] + "@test.example"
	s.Require().NoError(s.exec(t.System, `INSERT INTO mandate.sessions (token_hash, email, expires_at) VALUES (sha256($1::bytea), $2, now() + interval '1 day')`, []byte(t.Id.String()), reviewerEmail))
	s.Require().NoError(s.W.Kernel.Tenants.RemovePerson(t.Owner, t.ReviewerId))
	var sessions int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(), `SELECT count(*) FROM mandate.sessions WHERE email = $1`, reviewerEmail).Scan(&sessions))
	s.Require().Equal(1, sessions)
	s.Require().Equal([]string{"person.added", "person.removed"}, s.eventTypes(t.Owner, t.ReviewerId))

	// Removed means no role: the reviewer's own id is now nobody.
	_, err := s.W.Kernel.Tenants.FindPerson(t.System, reviewerEmail)
	s.code(err, fault.NotFound)
	s.refused(t.Reviewer, `UPDATE mandate.persons SET removed_at = now() WHERE id = $1`, t.OwnerId)
	s.code(s.W.Kernel.Tenants.RemovePerson(t.Owner, t.ReviewerId), fault.Conflict)
	people, err := s.W.Kernel.Tenants.ListPersons(t.Owner)
	s.Require().NoError(err)
	s.Require().Len(people, 2, "the row stays, so the chain still names them")

	// The last owner stays, and a removal cannot be undone.
	s.code(s.W.Kernel.Tenants.RemovePerson(t.Owner, t.OwnerId), fault.InvalidArgument)
	s.refused(t.Owner, `UPDATE mandate.persons SET removed_at = NULL WHERE id = $1`, t.ReviewerId)
	second, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "second@test.example", "Second", tenants.RoleOwner)
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.Tenants.RemovePerson(t.Owner, second.Id))

	// A removed person can be added again, as a new row.
	back, err := s.W.Kernel.Tenants.AddPerson(t.Owner, reviewerEmail, "Reviewer again", tenants.RoleReviewer)
	s.Require().NoError(err)
	s.Require().NotEqual(t.ReviewerId, back.Id)
	_, err = s.W.Kernel.Tenants.AddPerson(t.Owner, reviewerEmail, "Twice", tenants.RoleReviewer)
	s.code(err, fault.Conflict)

	// The system makes the first person of a tenant, never a later one.
	_, err = s.W.Kernel.Tenants.AddPerson(t.System, "planted@test.example", "Planted", tenants.RoleOwner)
	s.code(err, fault.PermissionDenied)
}

func (s *TenantsSuite) Test_TwoOwnersRemovingEachOtherLeaveOne() {
	for round := 0; round < 8; round++ {
		t := s.W.NewTenant(s.T(), "Acme")
		second, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "second@test.example", "Second", tenants.RoleOwner)
		s.Require().NoError(err)
		secondCtx := actor.AsPerson(context.Background(), t.Id, second.Id)
		var wg sync.WaitGroup
		var a, b error
		wg.Add(2)
		go func() { defer wg.Done(); a = s.W.Kernel.Tenants.RemovePerson(t.Owner, second.Id) }()
		go func() { defer wg.Done(); b = s.W.Kernel.Tenants.RemovePerson(secondCtx, t.OwnerId) }()
		wg.Wait()
		s.Require().True((a == nil) != (b == nil), "exactly one removal goes through: %v / %v", a, b)
		var owners int
		s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(),
			`SELECT count(*) FROM mandate.persons WHERE tenant_id = $1 AND role = 'owner' AND removed_at IS NULL`, t.Id).Scan(&owners))
		s.Require().Equal(1, owners, "a tenant never ends up without an owner")
	}
}

// The last owner is counted after the tenant's lock, which holds only
// when that count sees what committed before it. Under repeatable read
// both of two owners removing each other would see the other still there,
// so the database refuses people's changes under any other level, and
// db.InTx begins read committed whatever the default is.
func (s *TenantsSuite) Test_PeopleAreChangedUnderReadCommittedOnly() {
	ctx := context.Background()
	t := s.W.NewTenant(s.T(), "Acme")
	second, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "second@test.example", "Second", tenants.RoleOwner)
	s.Require().NoError(err)
	owners := func() int {
		var n int
		s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx,
			`SELECT count(*) FROM mandate.persons WHERE tenant_id = $1 AND role = 'owner' AND removed_at IS NULL`, t.Id).Scan(&n))
		return n
	}

	// Both take their snapshot with the scope, as db.InTx's first statement
	// would, before either removes the other.
	begin := func(by uuid.UUID) pgx.Tx {
		tx, err := s.W.Pg.App.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		s.Require().NoError(err)
		_, err = tx.Exec(ctx, `SELECT set_config('mandate.tenant_id', $1, true), set_config('mandate.actor_kind', 'person', true), set_config('mandate.actor_id', $2, true)`,
			t.Id.String(), by.String())
		s.Require().NoError(err)
		return tx
	}
	first, other := begin(t.OwnerId), begin(second.Id)
	defer first.Rollback(ctx) //nolint:errcheck
	defer other.Rollback(ctx) //nolint:errcheck
	remove := func(tx pgx.Tx, id uuid.UUID) error {
		if _, err := tx.Exec(ctx, `UPDATE mandate.persons SET removed_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	s.Require().ErrorContains(remove(first, second.Id), "read committed")
	s.Require().ErrorContains(remove(other, t.OwnerId), "read committed")
	s.Require().Equal(2, owners(), "neither removal under repeatable read went through")

	// A pool whose sessions default to repeatable read, as a DBA or a host
	// may set it: db.InTx still runs read committed, so the removal is
	// counted after the lock and taken.
	config, err := pgxpool.ParseConfig(s.W.Pg.AppUrl)
	s.Require().NoError(err)
	config.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	s.Require().NoError(err)
	defer pool.Close()
	var level string
	s.Require().NoError(pool.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&level))
	s.Require().Equal("repeatable read", level, "the pool's own default")
	s.Require().NoError(db.InTx(t.Owner, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(t.Owner, `SHOW transaction_isolation`).Scan(&level); err != nil {
			return err
		}
		_, err := tx.Exec(t.Owner, `UPDATE mandate.persons SET removed_at = now() WHERE id = $1`, second.Id)
		return err
	}))
	s.Require().Equal("read committed", level)
	s.Require().Equal(1, owners())
}

// Two removals of the same person can both read them as present. The one
// whose update then finds them removed is refused, as a removal after a
// removal is, and records nothing.
func (s *TenantsSuite) Test_ARemovalThatLosesTheRaceIsRefused() {
	t := s.W.NewTenant(s.T(), "Acme")
	err := s.W.CommitUnder(s.T(), t.Owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Owner, `UPDATE mandate.persons SET removed_at = now() WHERE id = $1`, t.ReviewerId)
		return err
	}, "name: RemovePerson ", func() error {
		return s.W.Kernel.Tenants.RemovePerson(t.Owner, t.ReviewerId)
	})
	s.code(err, fault.Conflict)
	s.Require().NotContains(s.eventTypes(t.Owner, t.ReviewerId), "person.removed", "the refused removal is not in the chain")
}

func (s *TenantsSuite) Test_ATenantIsCreatedWithItsOwner() {
	tenant, owner, err := s.W.Kernel.Tenants.CreateWithOwner(context.Background(), "Atomic", "First@Test.example", "First")
	s.Require().NoError(err)
	defer func() {
		s.Require().NoError(s.W.Pg.PurgeTenant(context.Background(), tenant.Id), "a purge removes append-only rows and the chain too")
	}()
	s.Require().Equal("first@test.example", owner.Email)
	ownerCtx := actor.AsPerson(context.Background(), tenant.Id, owner.Id)
	people, err := s.W.Kernel.Tenants.ListPersons(ownerCtx)
	s.Require().NoError(err)
	s.Require().Len(people, 1)
	s.Require().Equal([]string{"tenant.created"}, s.eventTypes(ownerCtx, tenant.Id))

	// A bad owner leaves no tenant behind.
	var before, after int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(), `SELECT count(*) FROM mandate.tenants`).Scan(&before))
	_, _, err = s.W.Kernel.Tenants.CreateWithOwner(context.Background(), "Broken", "not-an-email", "X")
	s.code(err, fault.InvalidArgument)
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(), `SELECT count(*) FROM mandate.tenants`).Scan(&after))
	s.Require().Equal(before, after)
}

func (s *TenantsSuite) Test_TheChainNamesItsActorAndContinuesItself() {
	t := s.W.NewTenant(s.T(), "Acme")
	zeros := make([]byte, 32)
	forged := `INSERT INTO mandate.events (tenant_id, seq, type, actor_kind, actor_id, payload, prev_hash, hash, created_at)
		SELECT $1, max(seq) + $4, 'proposal.approved', $2, $3, '{}', (array_agg(hash ORDER BY seq DESC))[1], $5, now() FROM mandate.events`
	// An agent cannot write an event as a person, even with the right
	// number and the right previous hash.
	s.refused(t.Agent, forged, t.Id, "person", t.OwnerId, 1, zeros)
	// Nor can anyone skip a number, which would break the chain for good.
	s.refused(t.Owner, forged, t.Id, "person", t.OwnerId, 2, zeros)
	// Or start from a hash that is not the last one.
	s.refused(t.Owner, `INSERT INTO mandate.events (tenant_id, seq, type, actor_kind, actor_id, payload, prev_hash, hash, created_at)
		SELECT $1, max(seq) + 1, 'proposal.approved', 'person', $2, '{}', $3, $3, now() FROM mandate.events`, t.Id, t.OwnerId, zeros)
	// Or date it before the event it follows, or after the database's clock.
	for _, at := range []string{"min(created_at) - interval '1 hour'", "clock_timestamp() + interval '1 hour'"} {
		s.refused(t.Owner, `INSERT INTO mandate.events (tenant_id, seq, type, actor_kind, actor_id, payload, prev_hash, hash, created_at)
			SELECT $1, max(seq) + 1, 'proposal.approved', 'person', $2, '{}', (array_agg(hash ORDER BY seq DESC))[1], $3, `+at+` FROM mandate.events`, t.Id, t.OwnerId, zeros)
	}
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		_, err := events.Verify(t.Owner, tx)
		return err
	}), "the refused inserts left the chain intact")
}

// The rows can be changed by someone with the database's superuser, who can
// turn the append-only triggers off. Verification must then name the first
// event that no longer fits.
func (s *TenantsSuite) Test_VerifyNamesTheFirstEventThatWasTamperedWith() {
	for _, c := range []struct {
		name, statement string
		at, count       int64
	}{
		{"a changed payload", `UPDATE mandate.events SET payload = payload || '{"forged": true}' WHERE tenant_id = $1 AND seq = 2`, 2, 2},
		{"a removed event", `DELETE FROM mandate.events WHERE tenant_id = $1 AND seq = 3`, 4, 3},
		{"a changed actor", `UPDATE mandate.events SET actor_kind = CASE actor_kind WHEN 'person' THEN 'system' ELSE 'person' END
			WHERE tenant_id = $1 AND seq = 4`, 4, 4},
	} {
		s.Run(c.name, func() {
			t := s.W.NewTenant(s.T(), "Acme")
			verify := func() (int64, error) {
				var count int64
				err := db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
					var err error
					count, err = events.Verify(t.Owner, tx)
					return err
				})
				return count, err
			}
			before, err := verify()
			s.Require().NoError(err, "the chain is intact before")
			s.Require().Greater(before, int64(4), "the tenant has events past the tampered one")

			s.Require().NoError(s.W.Pg.Tamper(context.Background(), c.statement, t.Id))
			count, err := verify()
			s.Require().ErrorIs(err, events.ErrBroken)
			s.Require().Contains(err.Error(), fmt.Sprintf("at seq %d", c.at))
			s.Require().Equal(c.count, count, "events read up to the broken one")
		})
	}
}

func (s *TenantsSuite) Test_PersonsAreAppendOnly() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.refused(t.Owner, `UPDATE mandate.persons SET name = 'Mallory' WHERE id = $1`, t.ReviewerId)
	s.refused(t.Owner, `DELETE FROM mandate.persons WHERE id = $1`, t.ReviewerId)
	s.refused(t.Owner, `DELETE FROM mandate.tenants WHERE id = $1`, t.Id)
	// The reviewer cannot make themselves an owner by inserting a second row either.
	s.refused(t.Reviewer, `INSERT INTO mandate.persons (id, tenant_id, email, name, role) VALUES (gen_random_uuid(), $1, 'me@test.example', 'Me', 'owner')`, t.Id)
}

// Any role can set mandate.allow_purge. Even with a DELETE right granted by
// mistake, the application role must not be able to remove append-only rows
// with it; only the table's owner, as in purge_tenant, may.
func (s *TenantsSuite) Test_ThePurgeSettingAloneDeletesNothing() {
	t := s.W.NewTenant(s.T(), "Acme")
	bg := context.Background()
	_, err := s.W.Pg.Owner.Exec(bg, `GRANT DELETE ON mandate.events TO mandate_app`)
	s.Require().NoError(err)
	defer func() {
		_, err := s.W.Pg.Owner.Exec(bg, `REVOKE DELETE ON mandate.events FROM mandate_app`)
		s.Require().NoError(err)
	}()
	var before int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(bg, `SELECT count(*) FROM mandate.events WHERE tenant_id = $1`, t.Id).Scan(&before))
	s.Require().Positive(before, "the tenant has events to try to delete")

	err = db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Owner, `SELECT set_config('mandate.allow_purge', 'on', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(t.Owner, `DELETE FROM mandate.events WHERE tenant_id = $1`, t.Id)
		return err
	})
	s.Require().Error(err, "the setting does not open the table to the app role")
	s.Require().Contains(err.Error(), "append-only", "the guard refused it, not a missing grant")

	var after int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(bg, `SELECT count(*) FROM mandate.events WHERE tenant_id = $1`, t.Id).Scan(&after))
	s.Require().Equal(before, after, "no event was removed")
}

func (s *TenantsSuite) Test_AnotherTenantSeesNothing() {
	a := s.W.NewTenant(s.T(), "A")
	b := s.W.NewTenant(s.T(), "B")
	owner, err := s.W.Kernel.Tenants.FindPerson(a.System, "owner-"+a.Id.String()[:8]+"@test.example")
	s.Require().NoError(err)
	_, err = s.W.Kernel.Tenants.FindPerson(b.System, owner.Email)
	s.code(err, fault.NotFound)
	people, err := s.W.Kernel.Tenants.ListPersons(b.Owner)
	s.Require().NoError(err)
	for _, p := range people {
		s.Require().NotEqual(owner.Id, p.Id)
	}
	_, err = s.W.Kernel.Tenants.Get(actor.AsPerson(context.Background(), b.Id, a.OwnerId))
	s.Require().NoError(err, "the scope is the tenant; a foreign person id simply has no role there")
	s.Require().Empty(s.eventTypes(b.System, a.Id), "A's chain is invisible from B")
	zeros := make([]byte, 32)
	s.refused(b.Owner, `INSERT INTO mandate.events (tenant_id, seq, type, actor_kind, payload, prev_hash, hash, created_at)
		VALUES ($1, 999, 'test.planted', 'person', '{}', $2, $2, now())`, a.Id, zeros)
	_, err = s.W.Kernel.Tenants.Get(context.Background())
	s.code(err, fault.InvalidArgument)
}

// The database binds a person to an identity only as the system, once, both
// columns together, and never for a removed person or on the way in,
// whatever SQL runs.
func (s *TenantsSuite) Test_TheDatabaseHoldsTheBinding() {
	t := s.W.NewTenant(s.T(), "Acme")
	const bind = `UPDATE mandate.persons SET issuer = 'https://id.example', subject = $2 WHERE id = $1`
	s.refused(t.Owner, bind, t.ReviewerId, "s-1")
	s.refused(t.Reviewer, bind, t.ReviewerId, "s-1")
	s.refused(t.Agent, bind, t.ReviewerId, "s-1")
	s.Require().Error(s.exec(t.System, `UPDATE mandate.persons SET issuer = 'https://id.example' WHERE id = $1`, t.ReviewerId), "never one column alone")
	s.refused(t.System, `UPDATE mandate.persons SET issuer = 'https://id.example', subject = 's-1', name = 'Changed' WHERE id = $1`, t.ReviewerId)
	s.Require().NoError(s.exec(t.System, bind, t.ReviewerId, "s-1"))
	s.refused(t.System, bind, t.ReviewerId, "s-2")
	s.refused(t.System, `UPDATE mandate.persons SET issuer = NULL, subject = NULL WHERE id = $1`, t.ReviewerId)

	// A second person with the same identity in the tenant is refused.
	second, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "second@test.example", "Second", "reviewer")
	s.Require().NoError(err)
	s.Require().Error(s.exec(t.System, bind, second.Id, "s-1"))

	// Added with an identity, it comes in unbound.
	s.Require().NoError(s.exec(t.Owner, `INSERT INTO mandate.persons (id, tenant_id, email, name, role, issuer, subject)
		VALUES (gen_random_uuid(), $1, 'planted@test.example', 'Planted', 'reviewer', 'https://id.example', 's-9')`, t.Id))
	var issuer *string
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Owner, `SELECT issuer FROM mandate.persons WHERE email = 'planted@test.example'`).Scan(&issuer)
	}))
	s.Require().Nil(issuer)

	// A removed person is never bound.
	s.Require().NoError(s.W.Kernel.Tenants.RemovePerson(t.Owner, second.Id))
	s.refused(t.System, bind, second.Id, "s-3")

	// Another tenant's system sees no one to bind.
	other := s.W.NewTenant(s.T(), "Other")
	s.Require().NoError(s.exec(other.System, bind, t.OwnerId, "s-4"))
	var ownerIssuer *string
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Owner, `SELECT issuer FROM mandate.persons WHERE id = $1`, t.OwnerId).Scan(&ownerIssuer)
	}))
	s.Require().Nil(ownerIssuer)
}
