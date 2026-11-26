package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/andreas-lindfalk/pgmandate/modules/auth/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/modules/tenants"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// How long a sign-in link may wait to be used.
const (
	SetupLinkLifetime  = 30 * time.Minute
	PersonLinkLifetime = 15 * time.Minute
)

// The paths the links open, in the web app. The token follows a "#", so it
// never reaches a server log, a proxy or a Referer.
const (
	PathSetup  = "/setup"
	PathSignIn = "/sign-in"
)

const (
	linkSetup  = "setup"
	linkPerson = "person"
)

// errLink is the one answer to a link that does not work, whatever the
// reason: unknown, used, expired, or a setup link once a tenant exists.
var errLink = fault.New(fault.Unauthenticated, "this link does not work: it is used, has expired, or pgmandate is already set up. Ask for a new one")

// SetupPending says nobody has set pgmandate up: there is no tenant.
func (s *Service) SetupPending(ctx context.Context) (bool, error) {
	var has bool
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		has, err = repo.New(tx).HasTenants(ctx)
		return err
	})
	return !has, err
}

// NewSetupLink makes the single-use token that sets up the first tenant and
// its owner, while there is no tenant, and voids any earlier one that is
// unused. made is false when a tenant exists, and there is no token.
func (s *Service) NewSetupLink(ctx context.Context) (token string, made bool, err error) {
	ctx = actor.WithActor(ctx, actor.Actor{Kind: actor.System})
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		if err := q.LockSetup(ctx); err != nil {
			return err
		}
		has, err := q.HasTenants(ctx)
		if err != nil || has {
			return err
		}
		if err := q.VoidSetupLinks(ctx); err != nil {
			return err
		}
		var hash []byte
		if token, hash, err = newToken(); err != nil {
			return err
		}
		made = true
		return q.InsertLink(ctx, repo.InsertLinkParams{TokenHash: hash, Purpose: linkSetup, LifetimeSeconds: int32(SetupLinkLifetime / time.Second)})
	})
	if err != nil {
		// A tenant made another way, as by the command line, between the
		// check and the insert: set up after all, which is no failure.
		if pending, pendingErr := s.SetupPending(ctx); pendingErr == nil && !pending {
			return "", false, nil
		}
		return "", false, fault.FromDb(err)
	}
	return token, made, nil
}

// NewPersonLink makes a single-use token that signs one person in, as the
// system of the tenant in ctx. The session it gives is pinned to them.
func (s *Service) NewPersonLink(ctx context.Context, personId uuid.UUID) (string, error) {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return "", fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	ctx = actor.AsSystem(ctx, tenantId)
	var token string
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var hash []byte
		var err error
		if token, hash, err = newToken(); err != nil {
			return err
		}
		return repo.New(tx).InsertLink(ctx, repo.InsertLinkParams{
			TokenHash: hash, Purpose: linkPerson, TenantId: &tenantId, PersonId: &personId, LifetimeSeconds: int32(PersonLinkLifetime / time.Second),
		})
	})
	if err != nil {
		return "", fault.FromDb(err)
	}
	return token, nil
}

// SetUp uses a setup link: it makes the first tenant and its owner, and
// signs the owner in, in one transaction that also uses the link. The
// session is pinned to the owner.
func (s *Service) SetUp(ctx context.Context, token, organisation, name, email string) (string, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return "", errLink
	}
	tenantId := uuid.New()
	scoped := actor.AsSystem(ctx, tenantId)
	var session string
	err := db.InTx(scoped, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		if err := q.LockSetup(scoped); err != nil {
			return err
		}
		if _, err := s.claim(scoped, q, hash, linkSetup); err != nil {
			return err
		}
		// Locked with the link, so two uses at once make one tenant.
		if has, err := q.HasTenants(scoped); err != nil || has {
			if err == nil {
				err = errLink
			}
			return err
		}
		_, owner, err := tenants.CreateWithOwnerIn(scoped, tx, tenantId, organisation, email, name)
		if err != nil {
			return err
		}
		session, err = s.insertSession(scoped, q, Session{Email: owner.Email, Name: owner.Name, PersonId: &owner.Id})
		return err
	})
	if err != nil {
		return "", fault.FromDb(err)
	}
	return session, nil
}

// RedeemLink uses a person's link and signs them in, pinned to them.
func (s *Service) RedeemLink(ctx context.Context, token string) (string, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return "", errLink
	}
	var session string
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		link, err := s.claim(ctx, q, hash, linkPerson)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT email, name FROM mandate.person_for_session($1)`, *link.PersonId)
		if err != nil {
			return err
		}
		contacts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[contact])
		if err != nil {
			return err
		}
		if len(contacts) == 0 {
			// Removed since the link was made.
			return errLink
		}
		session, err = s.insertSession(ctx, q, Session{Email: contacts[0].Email, Name: contacts[0].Name, PersonId: link.PersonId})
		return err
	})
	if err != nil {
		return "", fault.FromDb(err)
	}
	return session, nil
}

type contact struct {
	Email string
	Name  string
}

// claim locks a link and uses it, or refuses: unknown, of another purpose,
// used, or expired by the database's clock, all alike.
func (s *Service) claim(ctx context.Context, q *repo.Queries, hash []byte, purpose string) (repo.GetLinkForUpdateRow, error) {
	link, err := q.GetLinkForUpdate(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return link, errLink
	}
	if err != nil {
		return link, err
	}
	if link.Purpose != purpose || (purpose == linkPerson && link.PersonId == nil) {
		return link, errLink
	}
	used, err := q.UseLink(ctx, hash)
	if fault.CodeOf(fault.FromDb(err)) == fault.PermissionDenied {
		// The database's own refusal, as for a setup link once a tenant
		// exists: answered like any link that does not work.
		return link, errLink
	}
	if err != nil {
		return link, err
	}
	if used == 0 {
		return link, errLink
	}
	return link, nil
}
