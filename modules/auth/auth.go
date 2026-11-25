// Package auth signs people in: sessions, dev login on localhost, and OIDC
// against one provider. A session is global; the tenant is chosen per call.
package auth

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

	"github.com/andreas-lindfalk/pgmandate/modules/auth/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// CookieName is the session cookie the OIDC callback sets and the API
// accepts next to a bearer token.
const CookieName = "mandate_session"

type Config struct {
	// DevLogin lets anyone sign in with only an email. The server allows
	// it only when it listens on localhost.
	DevLogin bool
	// SessionTtl is how long a session lasts; zero means a week.
	SessionTtl time.Duration
	// PublicUrl is where the server is reached, for cookies and redirects.
	PublicUrl string
	// Oidc is nil when no provider is configured.
	Oidc *OidcConfig
}

type OidcConfig struct {
	Issuer       string
	ClientId     string
	ClientSecret string
	// RedirectUrl defaults to PublicUrl + /auth/oidc/callback.
	RedirectUrl string
}

type Session struct {
	Email string
	Name  string
}

// Membership is a tenant where the signed-in person is a person.
type Membership struct {
	TenantId   uuid.UUID
	TenantName string
	PersonId   uuid.UUID
	Role       string
}

type Service struct {
	pool   *pgxpool.Pool
	config Config
	now    func() time.Time
	oidc   *oidcClient
}

func NewService(pool *pgxpool.Pool, config Config) *Service {
	if config.SessionTtl <= 0 {
		config.SessionTtl = 7 * 24 * time.Hour
	}
	s := &Service{pool: pool, config: config, now: time.Now}
	if config.Oidc != nil {
		s.oidc = newOidcClient(s, *config.Oidc)
	}
	return s
}

// Config is the configuration the service runs with.
func (s *Service) Config() Config {
	return s.config
}

// DevLogin creates a session for the email without any credentials. It is
// refused unless dev login is enabled.
func (s *Service) DevLogin(ctx context.Context, email, name string) (string, error) {
	if !s.config.DevLogin {
		return "", fault.New(fault.PermissionDenied, "dev login is disabled")
	}
	return s.createSession(ctx, email, name)
}

// Authenticate returns the person behind a session token.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	hash, err := hashToken(token)
	if err != nil {
		return Session{}, err
	}
	var session Session
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		row, err := q.GetSession(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.Unauthenticated, "invalid or expired session")
		}
		if err != nil {
			return err
		}
		session = Session{Email: row.Email, Name: row.Name}
		return q.TouchSession(ctx, hash)
	})
	return session, err
}

func (s *Service) Logout(ctx context.Context, token string) error {
	hash, err := hashToken(token)
	if err != nil {
		return nil
	}
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		return repo.New(tx).DeleteSession(ctx, hash)
	})
}

// Memberships lists the tenants where the email is a person.
func (s *Service) Memberships(ctx context.Context, email string) ([]Membership, error) {
	var result []Membership
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, tenant_name, person_id, role FROM mandate.tenants_for_email($1)`, strings.ToLower(email))
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Membership, error) {
			var m Membership
			err := row.Scan(&m.TenantId, &m.TenantName, &m.PersonId, &m.Role)
			return m, err
		})
		return err
	})
	return result, err
}

// Membership is the person the email is in one tenant, or PermissionDenied.
func (s *Service) Membership(ctx context.Context, email string, tenantId uuid.UUID) (Membership, error) {
	all, err := s.Memberships(ctx, email)
	if err != nil {
		return Membership{}, err
	}
	for _, m := range all {
		if m.TenantId == tenantId {
			return m, nil
		}
	}
	return Membership{}, fault.New(fault.PermissionDenied, "you are not a person of that tenant")
}

// DeleteExpired removes sessions that have run out. For a periodic job.
func (s *Service) DeleteExpired(ctx context.Context) (int64, error) {
	var count int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		count, err = repo.New(tx).DeleteExpiredSessions(ctx)
		return err
	})
	return count, err
}

func (s *Service) createSession(ctx context.Context, email, name string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if !strings.Contains(email, "@") || len(email) > 200 {
		return "", fault.New(fault.InvalidArgument, "an email address is required")
	}
	if utf8.RuneCountInString(name) > 200 {
		name = string([]rune(name)[:200])
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		return repo.New(tx).InsertSession(ctx, repo.InsertSessionParams{
			TokenHash: hash[:], Email: email, Name: name, ExpiresAt: s.now().Add(s.config.SessionTtl),
		})
	})
	if err != nil {
		return "", fault.FromDb(err)
	}
	return token, nil
}

func hashToken(token string) ([]byte, error) {
	if len(token) < 32 || len(token) > 64 {
		return nil, fault.New(fault.Unauthenticated, "invalid session token")
	}
	hash := sha256.Sum256([]byte(token))
	return hash[:], nil
}
