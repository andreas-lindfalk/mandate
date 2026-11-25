// Package tenants holds the tenants and the persons who decide for them.
package tenants

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andreas-lindfalk/pgmandate/modules/tenants/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

const (
	RoleOwner    = "owner"
	RoleReviewer = "reviewer"
)

type Tenant struct {
	Id        uuid.UUID
	Name      string
	CreatedAt time.Time
}

type Person struct {
	Id        uuid.UUID
	Email     string
	Name      string
	Role      string
	CreatedAt time.Time
	RemovedAt *time.Time
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Create makes a tenant, as the system. The caller's ctx needs no scope.
func (s *Service) Create(ctx context.Context, name string) (Tenant, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 200 {
		return Tenant{}, fault.New(fault.InvalidArgument, "the name must be 1 to 200 characters")
	}
	tenant := Tenant{Id: uuid.New(), Name: name}
	ctx = actor.AsSystem(ctx, tenant.Id)
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		if err := q.InsertTenant(ctx, repo.InsertTenantParams{Id: tenant.Id, Name: name}); err != nil {
			return err
		}
		row, err := q.GetTenant(ctx, tenant.Id)
		if err != nil {
			return err
		}
		tenant.CreatedAt = row.CreatedAt
		_, err = events.Append(ctx, tx, events.Event{Type: "tenant.created", SubjectId: tenant.Id, Payload: map[string]any{"name": name}})
		return err
	})
	return tenant, fault.FromDb(err)
}

// CreateWithOwner makes a tenant and its first owner in one transaction,
// as the system, so a tenant never exists with nobody in it.
func (s *Service) CreateWithOwner(ctx context.Context, name, ownerEmail, ownerName string) (Tenant, Person, error) {
	name = strings.TrimSpace(name)
	ownerEmail = strings.ToLower(strings.TrimSpace(ownerEmail))
	ownerName = strings.TrimSpace(ownerName)
	switch {
	case name == "" || utf8.RuneCountInString(name) > 200:
		return Tenant{}, Person{}, fault.New(fault.InvalidArgument, "the name must be 1 to 200 characters")
	case !strings.Contains(ownerEmail, "@") || len(ownerEmail) > 200:
		return Tenant{}, Person{}, fault.New(fault.InvalidArgument, "the owner needs an email address")
	case ownerName == "" || utf8.RuneCountInString(ownerName) > 200:
		return Tenant{}, Person{}, fault.New(fault.InvalidArgument, "the owner's name must be 1 to 200 characters")
	}
	tenant := Tenant{Id: uuid.New(), Name: name}
	owner := Person{Id: uuid.New(), Email: ownerEmail, Name: ownerName, Role: RoleOwner}
	ctx = actor.AsSystem(ctx, tenant.Id)
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		if err := q.InsertTenant(ctx, repo.InsertTenantParams{Id: tenant.Id, Name: name}); err != nil {
			return err
		}
		if err := q.InsertPerson(ctx, repo.InsertPersonParams{Id: owner.Id, TenantId: tenant.Id, Email: ownerEmail, Name: ownerName, Role: RoleOwner}); err != nil {
			return err
		}
		t, err := q.GetTenant(ctx, tenant.Id)
		if err != nil {
			return err
		}
		p, err := q.GetPerson(ctx, owner.Id)
		if err != nil {
			return err
		}
		tenant.CreatedAt, owner.CreatedAt = t.CreatedAt, p.CreatedAt
		_, err = events.AppendMany(ctx, tx, []events.Event{
			{Type: "tenant.created", SubjectId: tenant.Id, Payload: map[string]any{"name": name}},
			{Type: "person.added", SubjectId: owner.Id, Payload: map[string]any{"name": ownerName, "role": RoleOwner}},
		})
		return err
	})
	return tenant, owner, fault.FromDb(err)
}

func (s *Service) Get(ctx context.Context) (Tenant, error) {
	var tenant Tenant
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		tenantId, ok := actor.TenantFrom(ctx)
		if !ok {
			return fault.New(fault.InvalidArgument, "no tenant in scope")
		}
		row, err := repo.New(tx).GetTenant(ctx, tenantId)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "tenant not found")
		}
		tenant = Tenant{Id: row.Id, Name: row.Name, CreatedAt: row.CreatedAt}
		return err
	})
	return tenant, err
}

// AddPerson adds a person to the tenant in scope. The system adds the first
// owner; after that only an owner adds people, which the database enforces.
func (s *Service) AddPerson(ctx context.Context, email, name, role string) (Person, error) {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return Person{}, fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	switch {
	case !strings.Contains(email, "@") || len(email) > 200:
		return Person{}, fault.New(fault.InvalidArgument, "an email address is required")
	case name == "" || utf8.RuneCountInString(name) > 200:
		return Person{}, fault.New(fault.InvalidArgument, "the name must be 1 to 200 characters")
	case role != RoleOwner && role != RoleReviewer:
		return Person{}, fault.New(fault.InvalidArgument, "the role is owner or reviewer")
	}
	person := Person{Id: uuid.New(), Email: email, Name: name, Role: role}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		if err := q.InsertPerson(ctx, repo.InsertPersonParams{Id: person.Id, TenantId: tenantId, Email: email, Name: name, Role: role}); err != nil {
			return err
		}
		row, err := q.GetPerson(ctx, person.Id)
		if err != nil {
			return err
		}
		person.CreatedAt = row.CreatedAt
		_, err = events.Append(ctx, tx, events.Event{Type: "person.added", SubjectId: person.Id, Payload: map[string]any{"name": name, "role": role}})
		return err
	})
	return person, fault.FromDb(err)
}

// RemovePerson takes a person's role away for good and ends their
// sessions. Only an owner may, and never the last owner; the database
// enforces both. The row stays, so the chain still names them.
func (s *Service) RemovePerson(ctx context.Context, id uuid.UUID) error {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.RequireOwner(ctx, tx); err != nil {
			return err
		}
		q := repo.New(tx)
		row, err := q.GetPerson(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "person not found")
		}
		if err != nil {
			return err
		}
		if row.RemovedAt != nil {
			return fault.New(fault.Conflict, "the person is already removed")
		}
		if _, err := q.RemovePerson(ctx, id); err != nil {
			return err
		}
		// Sessions are by email and span tenants, so they are left alone:
		// removal from this tenant takes effect on the next call, which
		// checks the person's membership, and their other tenants are
		// not this owner's to sign them out of.
		_, err = events.Append(ctx, tx, events.Event{Type: "person.removed", SubjectId: id, Payload: map[string]any{"role": row.Role}})
		return err
	})
	return fault.FromDb(err)
}

// FindPerson finds a person of the tenant in scope by email.
func (s *Service) FindPerson(ctx context.Context, email string) (Person, error) {
	var person Person
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := repo.New(tx).FindPerson(ctx, strings.ToLower(strings.TrimSpace(email)))
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "no such person in this tenant")
		}
		person = toPerson(row)
		return err
	})
	return person, err
}

func (s *Service) ListPersons(ctx context.Context) ([]Person, error) {
	var result []Person
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).ListPersons(ctx)
		for _, row := range rows {
			result = append(result, toPerson(row))
		}
		return err
	})
	return result, err
}

func toPerson(row repo.MandatePerson) Person {
	return Person{Id: row.Id, Email: row.Email, Name: row.Name, Role: row.Role, CreatedAt: row.CreatedAt, RemovedAt: row.RemovedAt}
}
