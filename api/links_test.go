package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/api"
	mandatev1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1/mandatev1connect"
	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/modules/auth"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// LinkSuite has a database of its own, with no tenant to begin with: the
// state a setup link is for.
type LinkSuite struct {
	suite.Suite
	W      *testfix.World
	auth   *auth.Service
	server *httptest.Server
	client mandatev1connect.AuthServiceClient
}

func TestLinkSuite(t *testing.T) {
	suite.Run(t, new(LinkSuite))
}

func (s *LinkSuite) SetupSuite() {
	w, err := testfix.Start(context.Background(), false)
	s.Require().NoError(err)
	s.W = w
	s.auth = auth.NewService(w.Pg.App, auth.Config{SessionTtl: time.Hour, PublicUrl: "http://127.0.0.1"})
	mux := http.NewServeMux()
	api.New(w.Kernel, s.auth).Mount(mux)
	s.server = httptest.NewServer(mux)
	s.client = mandatev1connect.NewAuthServiceClient(http.DefaultClient, s.server.URL)
}

func (s *LinkSuite) TearDownSuite() {
	s.server.Close()
	s.W.Close()
}

func (s *LinkSuite) code(err error, want connect.Code) {
	s.T().Helper()
	s.Require().Error(err)
	s.Require().Equal(want, connect.CodeOf(err), err.Error())
}

// setUp uses a setup link, and returns the answer with its headers.
func (s *LinkSuite) setUp(token, email string) (*setUpAnswer, error) {
	ctx, info := connect.NewClientContext(context.Background())
	resp, err := s.client.SetUp(ctx, &mandatev1.SetUpRequest{Token: token, Organisation: "Acme", Name: "Alice", Email: email})
	if err != nil {
		return nil, err
	}
	return &setUpAnswer{SetUpResponse: resp, header: info.ResponseHeader()}, nil
}

type setUpAnswer struct {
	*mandatev1.SetUpResponse
	header http.Header
}

// plantSetupLink puts a setup link in the table past the guard, as two
// servers starting at once could each have made one.
func (s *LinkSuite) plantSetupLink() string {
	s.T().Helper()
	token := strings.Repeat("p", 20) + uuid.NewString()[:23]
	hash := sha256.Sum256([]byte(token))
	_, err := s.W.Pg.Owner.Exec(context.Background(), `BEGIN; SET LOCAL session_replication_role = replica;
		INSERT INTO mandate.sign_in_links (token_hash, purpose, expires_at) VALUES ('\x`+hex.EncodeToString(hash[:])+`', 'setup', now() + interval '10 minutes'); COMMIT;`)
	s.Require().NoError(err)
	return token
}

// Tests in this suite run in order of their names: setup first, while there
// is no tenant, then everything that needs one.

// Two setup links used at once make one tenant; the owner can purge it,
// links and all.
func (s *LinkSuite) Test_0_TwoSetupLinksMakeOneTenant() {
	ctx := context.Background()
	first, second := s.plantSetupLink(), s.plantSetupLink()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, token := range []string{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.setUp(token, "alice@acme.example")
		}()
	}
	wg.Wait()
	s.Require().True((errs[0] == nil) != (errs[1] == nil), "one works, the other is refused: %v", errs)
	var tenantId string
	s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx, `SELECT id::text FROM mandate.tenants`).Scan(&tenantId))

	// The database refuses a setup link now, used or planted.
	third := s.plantSetupLink()
	_, err := s.setUp(third, "alice@acme.example")
	s.code(err, connect.CodeUnauthenticated)
	thirdHash := sha256.Sum256([]byte(third))
	err = db.InTx(actor.WithActor(ctx, actor.Actor{Kind: actor.System}), s.W.Pg.App, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE mandate.sign_in_links SET used_at = now() WHERE token_hash = $1`, thirdHash[:])
		return err
	})
	s.Require().Error(err, "the database itself refuses a setup link once a tenant exists")

	// The owner purges the tenant with its links, used and not.
	person, err := s.W.Kernel.Tenants.FindPerson(actor.AsSystem(ctx, uuid.MustParse(tenantId)), "alice@acme.example")
	s.Require().NoError(err)
	_, err = s.auth.NewPersonLink(actor.AsSystem(ctx, uuid.MustParse(tenantId)), person.Id)
	s.Require().NoError(err)
	s.Require().NoError(s.W.Pg.PurgeTenant(ctx, tenantId))
	// A used setup link names no tenant; it stays until it expires. The
	// next test starts without one.
	_, err = s.W.Pg.Owner.Exec(ctx, `BEGIN; SET LOCAL session_replication_role = replica; DELETE FROM mandate.sign_in_links WHERE purpose = 'setup'; COMMIT;`)
	s.Require().NoError(err)
	var left int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx, `SELECT count(*) FROM mandate.sign_in_links`).Scan(&left))
	s.Require().Zero(left)
}
func (s *LinkSuite) Test_1_TheFirstOwnerSetsUpWithALink() {
	ctx := context.Background()
	methods, err := s.client.SignInMethods(ctx, &mandatev1.SignInMethodsRequest{})
	s.Require().NoError(err)
	s.Require().True(methods.GetSetupPending())

	first, made, err := s.auth.NewSetupLink(ctx)
	s.Require().NoError(err)
	s.Require().True(made)
	second, _, err := s.auth.NewSetupLink(ctx)
	s.Require().NoError(err)
	_, err = s.setUp(first, "alice@acme.example")
	s.code(err, connect.CodeUnauthenticated)
	s.Require().Contains(err.Error(), "does not work", "a new link voids the last")

	// Only the hash is kept.
	var raw, hashed int
	hash := sha256.Sum256([]byte(second))
	s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE token_hash = $1), count(*) FILTER (WHERE token_hash = $2) FROM mandate.sign_in_links`,
		[]byte(second), hash[:]).Scan(&raw, &hashed))
	s.Require().Zero(raw)
	s.Require().Equal(1, hashed)

	// A form that is wrong leaves the link as it was.
	_, err = s.setUp(second, "not an email")
	s.code(err, connect.CodeInvalidArgument)

	// Two uses at once make one tenant.
	var wg sync.WaitGroup
	answers := make([]*setUpAnswer, 6)
	errs := make([]error, 6)
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i], errs[i] = s.setUp(second, "alice@acme.example")
		}()
	}
	wg.Wait()
	var won *setUpAnswer
	for i, err := range errs {
		if err == nil {
			s.Require().Nil(won, "only one use works")
			won = answers[i]
		} else {
			s.code(err, connect.CodeUnauthenticated)
		}
	}
	s.Require().NotNil(won)
	var tenants int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx, `SELECT count(*) FROM mandate.tenants`).Scan(&tenants))
	s.Require().Equal(1, tenants)
	s.Require().Contains(won.header.Get("Set-Cookie"), auth.CookieName+"=", "the browser is signed in by cookie")

	me, err := s.client.Me(as(won.GetToken(), ""), &mandatev1.MeRequest{})
	s.Require().NoError(err)
	s.Require().Len(me.GetTenants(), 1)
	s.Require().Equal("Acme", me.GetTenants()[0].GetTenantName())
	s.Require().Equal("owner", me.GetTenants()[0].GetRole())

	// Once there is a tenant, setup is over.
	_, made, err = s.auth.NewSetupLink(ctx)
	s.Require().NoError(err)
	s.Require().False(made)
	methods, err = s.client.SignInMethods(ctx, &mandatev1.SignInMethodsRequest{})
	s.Require().NoError(err)
	s.Require().False(methods.GetSetupPending())
}

func (s *LinkSuite) Test_2_APersonsLinkSignsThemInOnce() {
	ctx := context.Background()
	t := s.W.NewTenant(s.T(), "Beta")
	other := s.W.NewTenant(s.T(), "Gamma")
	bo, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "bo@beta.example", "Bo", "reviewer")
	s.Require().NoError(err)
	_, err = s.W.Kernel.Tenants.AddPerson(other.Owner, "bo@beta.example", "Bo", "reviewer")
	s.Require().NoError(err)

	token, err := s.auth.NewPersonLink(t.System, bo.Id)
	s.Require().NoError(err)
	signedIn, err := s.client.RedeemLink(ctx, &mandatev1.RedeemLinkRequest{Token: token})
	s.Require().NoError(err)
	me, err := s.client.Me(as(signedIn.GetToken(), ""), &mandatev1.MeRequest{})
	s.Require().NoError(err)
	s.Require().Len(me.GetTenants(), 1, "pinned to Bo in Beta, not every tenant with his email")
	s.Require().Equal("Beta", me.GetTenants()[0].GetTenantName())
	s.Require().Equal("bo@beta.example", me.GetEmail())
	_, err = mandatev1connect.NewPersonServiceClient(http.DefaultClient, s.server.URL).ListPersons(as(signedIn.GetToken(), other.Id.String()), &mandatev1.ListPersonsRequest{})
	s.code(err, connect.CodePermissionDenied)

	_, err = s.client.RedeemLink(ctx, &mandatev1.RedeemLinkRequest{Token: token})
	s.code(err, connect.CodeUnauthenticated)

	// Expired, by the database's clock.
	late, err := s.auth.NewPersonLink(t.System, bo.Id)
	s.Require().NoError(err)
	lateHash := sha256.Sum256([]byte(late))
	_, err = s.W.Pg.Owner.Exec(ctx, `BEGIN; SET LOCAL session_replication_role = replica;
		UPDATE mandate.sign_in_links SET created_at = now() - interval '20 minutes', expires_at = now() - interval '5 minutes' WHERE token_hash = '\x`+hex.EncodeToString(lateHash[:])+`'; COMMIT;`)
	s.Require().NoError(err)
	_, err = s.client.RedeemLink(ctx, &mandatev1.RedeemLinkRequest{Token: late})
	s.code(err, connect.CodeUnauthenticated)

	// A setup link is not a person's link, and the other way round.
	_, err = s.setUp(token, "x@beta.example")
	s.code(err, connect.CodeUnauthenticated)

	// Removed after the link was made: it signs nobody in, and a session
	// pinned to them reaches nothing.
	removable, err := s.auth.NewPersonLink(t.System, bo.Id)
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.Tenants.RemovePerson(t.Owner, bo.Id))
	_, err = s.client.RedeemLink(ctx, &mandatev1.RedeemLinkRequest{Token: removable})
	s.code(err, connect.CodeUnauthenticated)
	me, err = s.client.Me(as(signedIn.GetToken(), ""), &mandatev1.MeRequest{})
	s.Require().NoError(err)
	s.Require().Empty(me.GetTenants())
}

// The database holds the links whatever the Go code does.
func (s *LinkSuite) Test_3_TheDatabaseHoldsTheLinks() {
	t := s.W.NewTenant(s.T(), "Delta")
	exec := func(ctx context.Context, statement string, args ...any) error {
		return db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, statement, args...)
			return err
		})
	}
	refused := func(ctx context.Context, statement string, args ...any) {
		s.T().Helper()
		err := exec(ctx, statement, args...)
		s.Require().Error(err, statement)
		s.Require().Equal(fault.Name(fault.PermissionDenied), fault.Name(fault.CodeOf(fault.FromDb(err))), err.Error())
	}
	const setupLink = `INSERT INTO mandate.sign_in_links (token_hash, purpose, expires_at) VALUES (sha256(gen_random_uuid()::text::bytea), 'setup', now() + interval '10 minutes')`
	refused(actor.WithActor(context.Background(), actor.Actor{Kind: actor.System}), setupLink)
	refused(t.Owner, setupLink)
	const personLink = `INSERT INTO mandate.sign_in_links (token_hash, purpose, tenant_id, person_id, expires_at) VALUES (sha256(gen_random_uuid()::text::bytea), 'person', $1, $2, now() + interval '10 minutes')`
	refused(t.Owner, personLink, t.Id, t.ReviewerId)
	other := s.W.NewTenant(s.T(), "Epsilon")
	refused(other.System, personLink, t.Id, t.ReviewerId)
	s.Require().NoError(exec(t.System, personLink, t.Id, t.ReviewerId))
	s.Require().Error(exec(t.System, `INSERT INTO mandate.sign_in_links (token_hash, purpose, tenant_id, person_id, expires_at) VALUES (sha256('x'), 'person', $1, $2, now() + interval '2 hours')`, t.Id, t.ReviewerId), "an hour at most")

	token, err := s.auth.NewPersonLink(t.System, t.OwnerId)
	s.Require().NoError(err)
	hash := sha256.Sum256([]byte(token))
	_, err = s.client.RedeemLink(context.Background(), &mandatev1.RedeemLinkRequest{Token: token})
	s.Require().NoError(err)
	refused(t.System, `UPDATE mandate.sign_in_links SET used_at = now() WHERE token_hash = $1`, hash[:])
	refused(t.System, `DELETE FROM mandate.sign_in_links WHERE token_hash = $1`, hash[:])
	unused, err := s.auth.NewPersonLink(t.System, t.OwnerId)
	s.Require().NoError(err)
	unusedHash := sha256.Sum256([]byte(unused))
	refused(t.System, `UPDATE mandate.sign_in_links SET expires_at = now() + interval '50 minutes', used_at = now() WHERE token_hash = $1`, unusedHash[:])
	refused(t.System, `DELETE FROM mandate.sign_in_links WHERE token_hash = $1`, unusedHash[:])

	// Once expired, a link goes with the expired sessions.
	_, err = s.W.Pg.Owner.Exec(context.Background(), `BEGIN; SET LOCAL session_replication_role = replica;
		UPDATE mandate.sign_in_links SET created_at = now() - interval '30 minutes', expires_at = now() - interval '1 minute' WHERE token_hash = '\x`+hex.EncodeToString(unusedHash[:])+`'; COMMIT;`)
	s.Require().NoError(err)
	_, err = s.auth.DeleteExpired(context.Background())
	s.Require().NoError(err)
	var still int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(), `SELECT count(*) FROM mandate.sign_in_links WHERE token_hash = $1`, unusedHash[:]).Scan(&still))
	s.Require().Zero(still)
}
