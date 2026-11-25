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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andreas-lindfalk/pgmandate/modules/auth/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// CookieName is the session cookie the OIDC callback sets and the API
// accepts next to a bearer token.
const CookieName = "mandate_session"

// FlowCookieName is the cookie that carries a sign-in from its start to the
// provider's callback.
const FlowCookieName = "mandate_oidc"

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

// Session is who is signed in: the verified email and, for a provider's
// sign-in, its issuer and subject. Dev login has neither. A session from a
// sign-in link is pinned to one person instead.
type Session struct {
	Email   string
	Name    string
	Issuer  string
	Subject string
	// PersonId pins the session to one person: it reaches their tenant and
	// no other, and binds nothing.
	PersonId *uuid.UUID
}

// Membership is a tenant where the signed-in person is a person.
type Membership struct {
	TenantId   uuid.UUID
	TenantName string
	PersonId   uuid.UUID
	Role       string
	// Unbound says the person is not yet bound to an identity: the first
	// call in the tenant binds them to the session's.
	Unbound bool
}

type Service struct {
	pool   *pgxpool.Pool
	config Config
	oidc   *oidcClient
}

func NewService(pool *pgxpool.Pool, config Config) *Service {
	if config.SessionTtl <= 0 {
		config.SessionTtl = 7 * 24 * time.Hour
	}
	s := &Service{pool: pool, config: config}
	if config.Oidc != nil {
		s.oidc = newOidcClient(s, *config.Oidc)
	}
	return s
}

// secure says cookies are for https only, as the public URL is.
func (s *Service) secure() bool {
	return strings.HasPrefix(s.config.PublicUrl, "https://")
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
	return s.createSession(ctx, Session{Email: email, Name: name})
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
		session = Session{Email: row.Email, Name: row.Name, Issuer: deref(row.Issuer), Subject: deref(row.Subject), PersonId: row.PersonId}
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

// candidate is one person a sign-in may be, as tenants_for_sign_in says.
type candidate struct {
	Membership
	match string
}

// candidates lists the persons the session may be, at most one per tenant:
// the one bound to its identity first, then one with its email still
// unbound, then one with its email bound to another identity.
func (s *Service) candidates(ctx context.Context, session Session) ([]candidate, error) {
	if session.PersonId != nil {
		return s.pinned(ctx, *session.PersonId)
	}
	var issuer, subject *string
	if session.Issuer != "" {
		issuer, subject = &session.Issuer, &session.Subject
	}
	var all []candidate
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, tenant_name, person_id, role, match FROM mandate.tenants_for_sign_in($1, $2, $3)`,
			strings.ToLower(session.Email), issuer, subject)
		if err != nil {
			return err
		}
		all, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
			var c candidate
			err := row.Scan(&c.TenantId, &c.TenantName, &c.PersonId, &c.Role, &c.match)
			c.Unbound = c.match == matchUnbound
			return c, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	rank := map[string]int{matchBound: 0, matchUnbound: 1, matchOther: 2}
	best := map[uuid.UUID]int{}
	var out []candidate
	for _, c := range all {
		i, seen := best[c.TenantId]
		switch {
		case !seen:
			best[c.TenantId] = len(out)
			out = append(out, c)
		case rank[c.match] < rank[out[i].match]:
			out[i] = c
		}
	}
	return out, nil
}

// pinned is the one person a link's session is, while they are a person.
func (s *Service) pinned(ctx context.Context, personId uuid.UUID) ([]candidate, error) {
	var out []candidate
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, tenant_name, person_id, role FROM mandate.person_for_session($1)`, personId)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
			c := candidate{match: matchBound}
			err := row.Scan(&c.TenantId, &c.TenantName, &c.PersonId, &c.Role)
			return c, err
		})
		return err
	})
	return out, err
}

// How a person matches a sign-in.
const (
	matchBound   = "bound"
	matchUnbound = "unbound"
	matchOther   = "other"
)

// Memberships lists the tenants where the sign-in is a person: bound to its
// identity, or with its email and bound to nobody yet. Nothing is bound
// here; the first call in a tenant does that.
func (s *Service) Memberships(ctx context.Context, session Session) ([]Membership, error) {
	all, err := s.candidates(ctx, session)
	if err != nil {
		return nil, err
	}
	result := []Membership{}
	for _, c := range all {
		if c.match != matchOther {
			result = append(result, c.Membership)
		}
	}
	return result, nil
}

// Membership is the person the sign-in is in one tenant, or
// PermissionDenied. A person with the session's email who is bound to
// nobody yet is bound to the session's identity here, once, with a
// person.bound event in the same transaction.
func (s *Service) Membership(ctx context.Context, session Session, tenantId uuid.UUID) (Membership, error) {
	all, err := s.candidates(ctx, session)
	if err != nil {
		return Membership{}, err
	}
	for _, c := range all {
		if c.TenantId != tenantId {
			continue
		}
		switch {
		case c.match == matchOther:
			return Membership{}, fault.New(fault.PermissionDenied, "your email address belongs to another sign-in in this tenant; an owner can remove that person and add you again")
		case c.match == matchUnbound && session.Issuer != "":
			if err := s.bind(ctx, session, c.Membership); err != nil {
				return Membership{}, err
			}
			c.Unbound = false
		}
		return c.Membership, nil
	}
	return Membership{}, fault.New(fault.PermissionDenied, "you are not a person of that tenant")
}

// bind ties an unbound person to the session's identity, as the system in
// the person's tenant. Two first calls at once bind once: the second finds
// the person bound, to this identity or to another.
func (s *Service) bind(ctx context.Context, session Session, m Membership) error {
	scoped := actor.AsSystem(ctx, m.TenantId)
	err := db.InTx(scoped, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		bound, err := q.BindPerson(scoped, repo.BindPersonParams{Id: m.PersonId, Issuer: &session.Issuer, Subject: &session.Subject})
		if err != nil {
			return err
		}
		if bound == 0 {
			current, err := q.PersonIdentity(scoped, m.PersonId)
			if err != nil {
				return err
			}
			switch {
			case current.RemovedAt != nil || current.Issuer == nil:
				// Removed meanwhile.
				return fault.New(fault.PermissionDenied, "you are not a person of that tenant")
			case *current.Issuer == session.Issuer && deref(current.Subject) == session.Subject:
				return nil
			}
			return fault.New(fault.PermissionDenied, "your email address belongs to another sign-in in this tenant; an owner can remove that person and add you again")
		}
		// The log keeps the issuer, a provider's address, and the subject
		// only as a digest: an identifier of a person.
		subject, err := events.Digest(session.Subject)
		if err != nil {
			return err
		}
		_, err = events.Append(scoped, tx, events.Event{Type: "person.bound", SubjectId: m.PersonId, Payload: map[string]any{
			"issuer": session.Issuer, "subjectSha256": subject,
		}})
		return err
	})
	return fault.FromDb(err)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// DeleteExpired removes sessions that have run out. For a periodic job.
func (s *Service) DeleteExpired(ctx context.Context) (int64, error) {
	var count int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		q := repo.New(tx)
		if count, err = q.DeleteExpiredSessions(ctx); err != nil {
			return err
		}
		// Sign-in links that have expired, used or not, go with them.
		links, err := q.DeleteExpiredLinks(ctx)
		count += links
		return err
	})
	return count, err
}

func (s *Service) createSession(ctx context.Context, session Session) (string, error) {
	var token string
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		token, err = s.insertSession(ctx, repo.New(tx), session)
		return err
	})
	if err != nil {
		return "", fault.FromDb(err)
	}
	return token, nil
}

// insertSession makes a session in the caller's transaction and returns its
// token, which is never stored: only its hash is.
func (s *Service) insertSession(ctx context.Context, q *repo.Queries, session Session) (string, error) {
	email := strings.ToLower(strings.TrimSpace(session.Email))
	name := strings.TrimSpace(session.Name)
	if (session.Issuer == "") != (session.Subject == "") || len(session.Issuer) > 500 || len(session.Subject) > 255 {
		return "", fault.New(fault.InvalidArgument, "a sign-in has an issuer and a subject, or neither")
	}
	var issuer, subject *string
	if session.Issuer != "" {
		issuer, subject = &session.Issuer, &session.Subject
	}
	if !strings.Contains(email, "@") || len(email) > 200 {
		return "", fault.New(fault.InvalidArgument, "an email address is required")
	}
	if utf8.RuneCountInString(name) > 200 {
		name = string([]rune(name)[:200])
	}
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	if err := q.InsertSession(ctx, repo.InsertSessionParams{
		TokenHash: hash, Email: email, Name: name, Issuer: issuer, Subject: subject, PersonId: session.PersonId,
		Ttl: pgtype.Interval{Microseconds: s.config.SessionTtl.Microseconds(), Valid: true},
	}); err != nil {
		return "", err
	}
	return token, nil
}

// newToken is 32 random bytes as text, and the SHA-256 that is stored.
func newToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	return token, hash[:], nil
}

func hashToken(token string) ([]byte, error) {
	if len(token) < 32 || len(token) > 64 {
		return nil, fault.New(fault.Unauthenticated, "invalid session token")
	}
	hash := sha256.Sum256([]byte(token))
	return hash[:], nil
}
