// Package agents holds the agents of a tenant and their tokens. An agent is
// a principal of its own: it proposes in its own name, and every tool call
// it makes is recorded in the chain.
package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andreas-lindfalk/pgmandate/modules/agents/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

const (
	// TokenPrefix marks agent tokens, so they are recognizable in config
	// files and by secret scanners.
	TokenPrefix = "mnd_"
	secretBytes = 32
	maxDays     = 365
	maxName     = 80
	hintChars   = 4
)

type Agent struct {
	Id         uuid.UUID
	Name       string
	Department string
	CreatedBy  *uuid.UUID
	CreatedAt  time.Time
	RevokedAt  *time.Time
	// MaxPerMinute and MaxPerHour bound how many proposals the agent may
	// make; the database refuses the rest.
	MaxPerMinute int
	MaxPerHour   int
}

type Token struct {
	Id         uuid.UUID
	AgentId    uuid.UUID
	Hint       string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Grant is what a valid token allows: acting as one agent in one tenant.
type Grant struct {
	TokenId   uuid.UUID
	TenantId  uuid.UUID
	AgentId   uuid.UUID
	ExpiresAt time.Time
}

// Context scopes ctx to the grant's tenant as the agent.
func (g Grant) Context(ctx context.Context) context.Context {
	return actor.AsAgent(ctx, g.TenantId, g.AgentId)
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// SetNow replaces the clock, for tests of expiry.
func (s *Service) SetNow(now func() time.Time) {
	s.now = now
}

// Create registers an agent in the tenant in scope, with an optional
// department label. Only an owner may, which the database enforces.
func (s *Service) Create(ctx context.Context, name, department string) (Agent, error) {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return Agent{}, fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	name = strings.TrimSpace(name)
	department = strings.TrimSpace(department)
	if name == "" || utf8.RuneCountInString(name) > maxName {
		return Agent{}, fault.New(fault.InvalidArgument, "the name must be 1 to 80 characters")
	}
	if utf8.RuneCountInString(department) > 100 {
		return Agent{}, fault.New(fault.InvalidArgument, "the department can be at most 100 characters")
	}
	var dept *string
	if department != "" {
		dept = &department
	}
	agent := Agent{Id: uuid.New(), Name: name, Department: department}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		if err := q.InsertAgent(ctx, repo.InsertAgentParams{Id: agent.Id, TenantId: tenantId, Name: name, Department: dept}); err != nil {
			return err
		}
		row, err := q.GetAgent(ctx, agent.Id)
		if err != nil {
			return err
		}
		agent = toAgent(row)
		_, err = events.Append(ctx, tx, events.Event{Type: "agent.created", SubjectId: agent.Id, Payload: map[string]any{"name": name, "department": dept}})
		return err
	})
	return agent, fault.FromDb(err)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Agent, error) {
	var agent Agent
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := repo.New(tx).GetAgent(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "agent not found")
		}
		agent = toAgent(row)
		return err
	})
	return agent, err
}

func (s *Service) List(ctx context.Context) ([]Agent, error) {
	var result []Agent
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).ListAgents(ctx)
		for _, row := range rows {
			result = append(result, toAgent(row))
		}
		return err
	})
	return result, err
}

// Revoke ends an agent for good: its tokens stop working and its mandates
// no longer apply. Only an owner may; an agent revoked already is a Conflict.
func (s *Service) Revoke(ctx context.Context, id uuid.UUID) error {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.RequireOwner(ctx, tx); err != nil {
			return err
		}
		q := repo.New(tx)
		if _, err := q.GetAgent(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "agent not found")
		} else if err != nil {
			return err
		}
		changed, err := q.RevokeAgent(ctx, id)
		if err != nil {
			return err
		}
		if changed == 0 {
			return fault.New(fault.Conflict, "the agent is already revoked")
		}
		_, err = events.Append(ctx, tx, events.Event{Type: "agent.revoked", SubjectId: id, Payload: map[string]any{}})
		return err
	})
	return fault.FromDb(err)
}

// SetLimits sets how many proposals a live agent may make per minute and
// per hour. Only an owner may.
func (s *Service) SetLimits(ctx context.Context, id uuid.UUID, perMinute, perHour int) (Agent, error) {
	switch {
	case perMinute < 1 || perMinute > 10_000:
		return Agent{}, fault.New(fault.InvalidArgument, "the limit per minute is 1 to 10000")
	case perHour < perMinute || perHour > 100_000:
		return Agent{}, fault.New(fault.InvalidArgument, "the limit per hour is at least the limit per minute and at most 100000")
	}
	var result Agent
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.RequireOwner(ctx, tx); err != nil {
			return err
		}
		q := repo.New(tx)
		changed, err := q.SetAgentLimits(ctx, repo.SetAgentLimitsParams{Id: id, MaxPerMinute: int32(perMinute), MaxPerHour: int32(perHour)}) //nolint:gosec // bounded above
		if err != nil {
			return err
		}
		if changed == 0 {
			return fault.New(fault.NotFound, "no live agent with that id")
		}
		row, err := q.GetAgent(ctx, id)
		if err != nil {
			return err
		}
		result = toAgent(row)
		_, err = events.Append(ctx, tx, events.Event{Type: "agent.limits_set", SubjectId: id, Payload: map[string]any{"maxPerMinute": perMinute, "maxPerHour": perHour}})
		return err
	})
	return result, fault.FromDb(err)
}

// IssueToken makes a token for an agent. The secret is returned once; only
// its hash is stored. Only an owner may.
func (s *Service) IssueToken(ctx context.Context, agentId uuid.UUID, days int) (Token, string, error) {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return Token{}, "", fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	if days < 1 || days > maxDays {
		return Token{}, "", fault.New(fault.InvalidArgument, "a token is valid for 1 to 365 days")
	}
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Token{}, "", err
	}
	secret := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(secret))
	now := s.now().UTC()
	token := Token{Id: uuid.New(), AgentId: agentId, Hint: secret[len(secret)-hintChars:], CreatedAt: now, ExpiresAt: now.AddDate(0, 0, days)}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		agent, err := q.GetAgent(ctx, agentId)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "agent not found")
		}
		if err != nil {
			return err
		}
		if agent.RevokedAt != nil {
			return fault.New(fault.PermissionDenied, "the agent is revoked")
		}
		if err := q.InsertToken(ctx, repo.InsertTokenParams{
			Id: token.Id, TenantId: tenantId, AgentId: agentId, TokenHash: hash[:], Hint: token.Hint,
			CreatedAt: token.CreatedAt, ExpiresAt: token.ExpiresAt,
		}); err != nil {
			return err
		}
		_, err = events.Append(ctx, tx, events.Event{
			Type: "agent.token_issued", SubjectId: agentId,
			Payload: map[string]any{"tokenId": token.Id, "hint": token.Hint, "expiresAt": token.ExpiresAt.UTC().Format(time.RFC3339)},
		})
		return err
	})
	if err != nil {
		return Token{}, "", fault.FromDb(err)
	}
	return token, secret, nil
}

func (s *Service) ListTokens(ctx context.Context, agentId uuid.UUID) ([]Token, error) {
	var result []Token
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).ListTokens(ctx, agentId)
		for _, row := range rows {
			result = append(result, Token{
				Id: row.Id, AgentId: row.AgentId, Hint: row.Hint, CreatedAt: row.CreatedAt,
				ExpiresAt: row.ExpiresAt, LastUsedAt: row.LastUsedAt, RevokedAt: row.RevokedAt,
			})
		}
		return err
	})
	return result, err
}

// RevokeToken ends a token for good. Only an owner may.
func (s *Service) RevokeToken(ctx context.Context, id uuid.UUID) error {
	if a, ok := actor.From(ctx); !ok || a.Kind != actor.Person {
		return fault.New(fault.PermissionDenied, "only a person revokes agent tokens")
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.RequireOwner(ctx, tx); err != nil {
			return err
		}
		q := repo.New(tx)
		token, err := q.GetToken(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "token not found")
		}
		if err != nil {
			return err
		}
		if token.RevokedAt != nil {
			return fault.New(fault.Conflict, "the token is already revoked")
		}
		if err := q.RevokeToken(ctx, id); err != nil {
			return err
		}
		_, err = events.Append(ctx, tx, events.Event{Type: "agent.token_revoked", SubjectId: token.AgentId, Payload: map[string]any{"tokenId": id}})
		return err
	})
	return fault.FromDb(err)
}

// Authenticate turns a secret into a grant. The token must be live and its
// agent not revoked. The caller's ctx needs no scope.
func (s *Service) Authenticate(ctx context.Context, secret string) (Grant, error) {
	if !strings.HasPrefix(secret, TokenPrefix) || len(secret) > 100 {
		return Grant{}, fault.New(fault.Unauthenticated, "invalid agent token")
	}
	hash := sha256.Sum256([]byte(secret))
	var grant Grant
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id, tenant_id, agent_id, expires_at FROM mandate.resolve_agent_token($1)`, hash[:]).
			Scan(&grant.TokenId, &grant.TenantId, &grant.AgentId, &grant.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.Unauthenticated, "invalid, expired or revoked agent token")
		}
		return err
	})
	if err != nil {
		return Grant{}, err
	}
	scoped := grant.Context(ctx)
	if err := db.InTx(scoped, s.pool, func(tx pgx.Tx) error {
		return repo.New(tx).TouchToken(scoped, grant.TokenId)
	}); err != nil {
		return Grant{}, err
	}
	return grant, nil
}

// Called records a tool call in the chain, as the agent (ctx must be scoped
// by the grant). Free text is never stored, only what the tool describes.
func (s *Service) Called(ctx context.Context, grant Grant, tool string, arguments map[string]any, failed bool) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := events.Append(ctx, tx, events.Event{
			Type: "agent.tool_called", SubjectId: grant.AgentId,
			Payload: map[string]any{"tool": tool, "arguments": arguments, "failed": failed, "tokenId": grant.TokenId},
		})
		return err
	})
}

func toAgent(row repo.MandateAgent) Agent {
	a := Agent{
		Id: row.Id, Name: row.Name, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, RevokedAt: row.RevokedAt,
		MaxPerMinute: int(row.MaxPerMinute), MaxPerHour: int(row.MaxPerHour),
	}
	if row.Department != nil {
		a.Department = *row.Department
	}
	return a
}
