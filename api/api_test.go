package api_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"
	"google.golang.org/protobuf/proto"

	"github.com/andreas-lindfalk/pgmandate/api"
	mandatev1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1/mandatev1connect"
	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/modules/auth"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type ApiSuite struct {
	suite.Suite
	W         *testfix.World
	provider  *fakeProvider
	server    *httptest.Server
	sessions  *auth.Service
	auth      mandatev1connect.AuthServiceClient
	persons   mandatev1connect.PersonServiceClient
	agents    mandatev1connect.AgentServiceClient
	actions   mandatev1connect.ActionServiceClient
	mandates  mandatev1connect.MandateServiceClient
	props     mandatev1connect.ProposalServiceClient
	evidence  mandatev1connect.EvidenceServiceClient
	notifiers mandatev1connect.NotifierServiceClient
}

func TestApiSuite(t *testing.T) {
	suite.Run(t, new(ApiSuite))
}

func (s *ApiSuite) SetupSuite() {
	w, err := testfix.Start(context.Background(), false)
	s.Require().NoError(err)
	s.W = w
	s.provider = newFakeProvider(s.T())
	authService := auth.NewService(w.Pg.App, auth.Config{
		DevLogin: true, SessionTtl: time.Hour, PublicUrl: "http://127.0.0.1",
		Oidc: &auth.OidcConfig{Issuer: s.provider.server.URL, ClientId: "mandate-web", ClientSecret: "shh"},
	})
	s.sessions = authService
	mux := http.NewServeMux()
	api.New(w.Kernel, authService).Mount(mux)
	s.server = httptest.NewServer(mux)
	client := http.DefaultClient
	s.auth = mandatev1connect.NewAuthServiceClient(client, s.server.URL)
	s.persons = mandatev1connect.NewPersonServiceClient(client, s.server.URL)
	s.agents = mandatev1connect.NewAgentServiceClient(client, s.server.URL)
	s.actions = mandatev1connect.NewActionServiceClient(client, s.server.URL)
	s.mandates = mandatev1connect.NewMandateServiceClient(client, s.server.URL)
	s.props = mandatev1connect.NewProposalServiceClient(client, s.server.URL)
	s.evidence = mandatev1connect.NewEvidenceServiceClient(client, s.server.URL)
	s.notifiers = mandatev1connect.NewNotifierServiceClient(client, s.server.URL)
}

func (s *ApiSuite) TearDownSuite() {
	s.server.Close()
	s.provider.server.Close()
	s.W.Close()
}

// as scopes a call to a session token and a tenant.
func as(token, tenantId string) context.Context {
	ctx, info := connect.NewClientContext(context.Background())
	if token != "" {
		info.RequestHeader().Set("Authorization", "Bearer "+token)
	}
	if tenantId != "" {
		info.RequestHeader().Set(auth.TenantHeader, tenantId)
	}
	return ctx
}

func (s *ApiSuite) login(email string) string {
	s.T().Helper()
	resp, err := s.auth.DevLogin(context.Background(), &mandatev1.DevLoginRequest{Email: email, Name: "Person"})
	s.Require().NoError(err)
	return resp.GetToken()
}

func (s *ApiSuite) code(err error, want connect.Code) {
	s.T().Helper()
	s.Require().Error(err)
	s.Require().Equal(want.String(), connect.CodeOf(err).String(), err.Error())
}

func (s *ApiSuite) Test_SignInAndTenants() {
	t := s.W.NewTenant(s.T(), "Acme")
	ownerEmail := "owner-" + t.Id.String()[:8] + "@test.example"
	loginCtx, loginInfo := connect.NewClientContext(context.Background())
	resp, err := s.auth.DevLogin(loginCtx, &mandatev1.DevLoginRequest{Email: ownerEmail, Name: "Person"})
	s.Require().NoError(err)
	token := resp.GetToken()
	s.Require().Contains(loginInfo.ResponseHeader().Get("Set-Cookie"), auth.CookieName+"="+token, "dev login signs the browser in by cookie")
	s.Require().Contains(loginInfo.ResponseHeader().Get("Set-Cookie"), "HttpOnly")

	me, err := s.auth.Me(as(token, ""), &mandatev1.MeRequest{})
	s.Require().NoError(err)
	s.Require().Equal(ownerEmail, me.GetEmail())
	s.Require().True(me.GetDevLogin())
	s.Require().True(me.GetOidc())
	s.Require().Len(me.GetTenants(), 1)
	s.Require().Equal(t.Id.String(), me.GetTenants()[0].GetTenantId())
	s.Require().Equal("owner", me.GetTenants()[0].GetRole())

	_, err = s.auth.Me(as("", ""), &mandatev1.MeRequest{})
	s.code(err, connect.CodeUnauthenticated)
	methods, err := s.auth.SignInMethods(as("", ""), &mandatev1.SignInMethodsRequest{})
	s.Require().NoError(err, "a signed-out visitor can ask how to sign in")
	s.Require().True(methods.GetDevLogin())
	s.Require().True(methods.GetOidc())
	_, err = s.auth.Me(as("not-a-token-not-a-token-not-a-token", ""), &mandatev1.MeRequest{})
	s.code(err, connect.CodeUnauthenticated)
	_, err = s.persons.ListPersons(as(token, ""), &mandatev1.ListPersonsRequest{})
	s.code(err, connect.CodeInvalidArgument)
	_, err = s.persons.ListPersons(as(token, uuid.NewString()), &mandatev1.ListPersonsRequest{})
	s.code(err, connect.CodePermissionDenied)
	list, err := s.persons.ListPersons(as(token, t.Id.String()), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
	s.Require().Len(list.GetPersons(), 2)

	// A stranger with a session has no tenants and no access.
	stranger := s.login("stranger@test.example")
	none, err := s.auth.Me(as(stranger, ""), &mandatev1.MeRequest{})
	s.Require().NoError(err)
	s.Require().Empty(none.GetTenants())
	_, err = s.persons.ListPersons(as(stranger, t.Id.String()), &mandatev1.ListPersonsRequest{})
	s.code(err, connect.CodePermissionDenied)

	// The cookie works as well as the header.
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	serverUrl, _ := url.Parse(s.server.URL)
	jar.SetCookies(serverUrl, []*http.Cookie{{Name: auth.CookieName, Value: token}})
	withCookie := mandatev1connect.NewAuthServiceClient(&http.Client{Jar: jar}, s.server.URL)
	me, err = withCookie.Me(context.Background(), &mandatev1.MeRequest{})
	s.Require().NoError(err)
	s.Require().Equal(ownerEmail, me.GetEmail())

	_, err = s.auth.Logout(as(token, ""), &mandatev1.LogoutRequest{})
	s.Require().NoError(err)
	_, err = s.auth.Me(as(token, ""), &mandatev1.MeRequest{})
	s.code(err, connect.CodeUnauthenticated)
}

// A session opens nothing once it has expired, by header or by cookie, and
// clearing out expired sessions takes it and leaves a live one.
func (s *ApiSuite) Test_AnExpiredSessionIsRefused() {
	ctx := context.Background()
	expiring := s.login("expiring@test.example")
	live := s.login("live@test.example")
	_, err := s.auth.Me(as(expiring, ""), &mandatev1.MeRequest{})
	s.Require().NoError(err)

	expiringHash := sha256.Sum256([]byte(expiring))
	liveHash := sha256.Sum256([]byte(live))
	tag, err := s.W.Pg.Owner.Exec(ctx, `UPDATE mandate.sessions SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, expiringHash[:])
	s.Require().NoError(err)
	s.Require().Equal(int64(1), tag.RowsAffected())

	_, err = s.auth.Me(as(expiring, ""), &mandatev1.MeRequest{})
	s.code(err, connect.CodeUnauthenticated)
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	serverUrl, err := url.Parse(s.server.URL)
	s.Require().NoError(err)
	jar.SetCookies(serverUrl, []*http.Cookie{{Name: auth.CookieName, Value: expiring}})
	_, err = mandatev1connect.NewAuthServiceClient(&http.Client{Jar: jar}, s.server.URL).Me(ctx, &mandatev1.MeRequest{})
	s.code(err, connect.CodeUnauthenticated)
	_, err = s.auth.Me(as(live, ""), &mandatev1.MeRequest{})
	s.Require().NoError(err, "a session that has not expired still opens")

	deleted, err := s.sessions.DeleteExpired(ctx)
	s.Require().NoError(err)
	s.Require().GreaterOrEqual(deleted, int64(1))
	count := func(hash [32]byte) int {
		var n int
		s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx, `SELECT count(*) FROM mandate.sessions WHERE token_hash = $1`, hash[:]).Scan(&n))
		return n
	}
	s.Require().Zero(count(expiringHash), "the expired session is gone")
	s.Require().Equal(1, count(liveHash), "the live session stays")
}

// A removed definition hides the builtin of the same key: the list does
// not offer it as builtin-only, and it shows only as the removed definition
// when removed ones are asked for.
func (s *ApiSuite) Test_ARemovedDefinitionHidesTheBuiltin() {
	t := s.W.NewTenant(s.T(), "Acme")
	owner := as(s.login("owner-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	listed := func(includeRemoved bool) map[string]*mandatev1.Action {
		resp, err := s.actions.ListActions(owner, &mandatev1.ListActionsRequest{IncludeRemoved: includeRemoved})
		s.Require().NoError(err)
		result := map[string]*mandatev1.Action{}
		for _, a := range resp.GetActions() {
			s.Require().NotContains(result, a.GetKey(), "one entry per key")
			result[a.GetKey()] = a
		}
		return result
	}
	s.Require().True(listed(false)["test.low"].GetBuiltinOnly())

	_, err := s.actions.DefineAction(owner, &mandatev1.DefineActionRequest{Key: "test.low", Description: "Defined here", Risk: "low", MayExecute: true, Connector: "builtin"})
	s.Require().NoError(err)
	s.Require().False(listed(false)["test.low"].GetBuiltinOnly())
	_, err = s.actions.RemoveAction(owner, &mandatev1.RemoveActionRequest{Key: "test.low"})
	s.Require().NoError(err)

	current := listed(false)
	s.Require().NotContains(current, "test.low", "a removed definition does not bring the builtin back")
	s.Require().True(current["test.medium"].GetBuiltinOnly(), "other builtins stay")
	all := listed(true)
	s.Require().Contains(all, "test.low")
	s.Require().False(all["test.low"].GetBuiltinOnly())
	s.Require().NotNil(all["test.low"].GetRemovedAt(), "it shows as the removed definition")
}

func (s *ApiSuite) Test_OwnerRunsTheTenantAndReviewerDecides() {
	t := s.W.NewTenant(s.T(), "Acme")
	owner := as(s.login("owner-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	reviewer := as(s.login("reviewer-"+t.Id.String()[:8]+"@test.example"), t.Id.String())

	added, err := s.persons.AddPerson(owner, &mandatev1.AddPersonRequest{Email: "second@test.example", Name: "Second", Role: "reviewer"})
	s.Require().NoError(err)
	s.Require().Equal("second@test.example", added.GetPerson().GetEmail())
	_, err = s.persons.AddPerson(reviewer, &mandatev1.AddPersonRequest{Email: "third@test.example", Name: "Third", Role: "reviewer"})
	s.code(err, connect.CodePermissionDenied)

	agent, err := s.agents.CreateAgent(owner, &mandatev1.CreateAgentRequest{Name: "support-bot", Department: "Support"})
	s.Require().NoError(err)
	s.Require().Equal("Support", agent.GetAgent().GetDepartment())
	s.Require().Equal("dry_run", agent.GetAgent().GetMode(), "a new agent starts in dry run")
	_, err = s.agents.SetAgentMode(reviewer, &mandatev1.SetAgentModeRequest{Id: agent.GetAgent().GetId(), Mode: "live"})
	s.code(err, connect.CodePermissionDenied)
	_, err = s.agents.SetAgentMode(owner, &mandatev1.SetAgentModeRequest{Id: agent.GetAgent().GetId(), Mode: "sometimes"})
	s.code(err, connect.CodeInvalidArgument)
	live, err := s.agents.SetAgentMode(owner, &mandatev1.SetAgentModeRequest{Id: agent.GetAgent().GetId(), Mode: "live"})
	s.Require().NoError(err)
	s.Require().Equal("live", live.GetAgent().GetMode())
	created, err := s.agents.CreateAgent(owner, &mandatev1.CreateAgentRequest{Name: "live-bot", Mode: "live"})
	s.Require().NoError(err)
	s.Require().Equal("live", created.GetAgent().GetMode(), "an owner may create one live")

	// Waiting dry runs are kept apart from the queue a person works.
	dry := s.W.DryRunAgent(s.T(), t)
	dryProposal, _, err := s.W.Kernel.Proposals.Propose(dry.Agent, proposals.Input{Action: "test.manual", Title: "Trial", Payload: json.RawMessage(`{}`), IdempotencyKey: "dry-1", Rationale: "Why."})
	s.Require().NoError(err)
	livePending, err := s.props.ListProposals(reviewer, &mandatev1.ListProposalsRequest{Status: "pending", DryRun: proto.Bool(false)})
	s.Require().NoError(err)
	for _, p := range livePending.GetProposals() {
		s.Require().False(p.GetDryRun())
	}
	dryPending, err := s.props.ListProposals(reviewer, &mandatev1.ListProposalsRequest{Status: "pending", DryRun: proto.Bool(true)})
	s.Require().NoError(err)
	s.Require().Len(dryPending.GetProposals(), 1)
	s.Require().Equal(int64(1), dryPending.GetTotal())
	s.Require().True(dryPending.GetProposals()[0].GetDryRun())
	_, err = s.props.ApproveProposal(owner, &mandatev1.ApproveProposalRequest{Id: dryProposal.Id.String()})
	s.code(err, connect.CodeFailedPrecondition)
	dismissed, err := s.props.DismissProposal(reviewer, &mandatev1.DismissProposalRequest{Id: dryProposal.Id.String(), Note: "seen"})
	s.Require().NoError(err)
	s.Require().Equal("dismissed", dismissed.GetProposal().GetStatus())
	issued, err := s.agents.IssueToken(owner, &mandatev1.IssueTokenRequest{AgentId: agent.GetAgent().GetId(), Days: 30})
	s.Require().NoError(err)
	s.Require().True(strings.HasPrefix(issued.GetSecret(), "mnd_"))
	tokens, err := s.agents.ListTokens(owner, &mandatev1.ListTokensRequest{AgentId: agent.GetAgent().GetId()})
	s.Require().NoError(err)
	s.Require().Len(tokens.GetTokens(), 1)
	_, err = s.agents.IssueToken(reviewer, &mandatev1.IssueTokenRequest{AgentId: agent.GetAgent().GetId(), Days: 30})
	s.code(err, connect.CodePermissionDenied)
	_, err = s.agents.IssueToken(owner, &mandatev1.IssueTokenRequest{AgentId: "nope", Days: 30})
	s.code(err, connect.CodeInvalidArgument)

	defined, err := s.actions.DefineAction(owner, &mandatev1.DefineActionRequest{
		Key: "tickets.create", Description: "Open a ticket", Risk: "low", MayExecute: true,
		SchemaJson: `{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}`,
		Connector:  "webhook", ConnectorUrl: "https://helpdesk.example/hooks",
	})
	s.Require().NoError(err)
	s.Require().True(strings.HasPrefix(defined.GetAction().GetAudience(), "pgm:"), "the action's audience is shown, not a secret")
	actionsList, err := s.actions.ListActions(reviewer, &mandatev1.ListActionsRequest{})
	s.Require().NoError(err)
	keys := map[string]bool{}
	for _, e := range actionsList.GetActions() {
		keys[e.GetKey()] = e.GetBuiltinOnly()
	}
	s.Require().False(keys["tickets.create"], "defined")
	s.Require().True(keys["test.low"], "builtin from the embedded host, not defined")
	_, err = s.actions.DefineAction(reviewer, &mandatev1.DefineActionRequest{Key: "x.y", Risk: "low", Connector: "builtin"})
	s.code(err, connect.CodePermissionDenied)
	// Seconds too large for a duration are refused, not wrapped around:
	// 2^55 seconds is 0 modulo 2^64 nanoseconds, which would read as the
	// default, and 2^55 + 3600 as one hour.
	for _, n := range []int64{1 << 55, 1<<55 + 3600, -(1 << 55)} {
		_, err = s.actions.DefineAction(owner, &mandatev1.DefineActionRequest{Key: "x.wrap", Risk: "low", Connector: "builtin", WaitSeconds: n})
		s.code(err, connect.CodeInvalidArgument)
		s.Require().Contains(err.Error(), "between 1 minute and 30 days", "wait_seconds %d", n)
		_, err = s.actions.DefineAction(owner, &mandatev1.DefineActionRequest{Key: "x.wrap", Risk: "low", Connector: "builtin", DeliverSeconds: n})
		s.code(err, connect.CodeInvalidArgument)
		s.Require().Contains(err.Error(), "within 2 minutes to 7 days", "deliver_seconds %d", n)
	}

	granted, err := s.mandates.GrantMandate(owner, &mandatev1.GrantMandateRequest{
		AgentId: agent.GetAgent().GetId(), Actions: []string{"tickets.*"}, MaxRisk: "low", BudgetCount: 10, PolicyRef: "ACME-POLICY", Days: 30, Department: "Support",
	})
	s.Require().NoError(err)
	s.Require().Equal("Support", granted.GetMandate().GetDepartment())
	s.Require().Empty(granted.GetMandate().GetCondition())
	mandatesList, err := s.mandates.ListMandates(reviewer, &mandatev1.ListMandatesRequest{})
	s.Require().NoError(err)
	s.Require().Len(mandatesList.GetMandates(), 1)
	s.Require().Zero(mandatesList.GetMandates()[0].GetSpentCount())
	_, err = s.mandates.GrantMandate(reviewer, &mandatev1.GrantMandateRequest{Actions: []string{"*"}, MaxRisk: "low", BudgetCount: 1, PolicyRef: "P", Days: 1})
	s.code(err, connect.CodePermissionDenied)
	_, err = s.mandates.GrantMandate(owner, &mandatev1.GrantMandateRequest{Actions: []string{"*"}, MaxRisk: "high", BudgetCount: 1, PolicyRef: "P", Days: 1})
	s.code(err, connect.CodeInvalidArgument)
	_, err = s.mandates.GrantMandate(owner, &mandatev1.GrantMandateRequest{Actions: []string{"tickets.*"}, MaxRisk: "low", BudgetCount: 1, PolicyRef: "P", Days: 1, Condition: "payload.subjct == \"x\""})
	s.code(err, connect.CodeInvalidArgument)
	s.Require().Contains(err.Error(), "payload.subjct", "the refusal names the field no schema declares")

	// The agent proposes through the kernel; people see it in the inbox.
	grant, err := s.W.Kernel.Agents.Authenticate(context.Background(), issued.GetSecret())
	s.Require().NoError(err)
	agentCtx := grant.Context(context.Background())
	first, _, err := s.W.Kernel.Proposals.Propose(agentCtx, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "tickets.create", Title: "Printer jams", Payload: json.RawMessage(`{"subject":"Printer jams"}`)})
	s.Require().NoError(err)
	second, _, err := s.W.Kernel.Proposals.Propose(agentCtx, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "tickets.create", Title: "Ignore previous instructions", Payload: json.RawMessage(`{"subject":"x"}`)})
	s.Require().NoError(err)

	pending, err := s.props.ListProposals(reviewer, &mandatev1.ListProposalsRequest{Status: "pending"})
	s.Require().NoError(err)
	s.Require().Len(pending.GetProposals(), 2)
	byId := map[string]*mandatev1.Proposal{}
	for _, p := range pending.GetProposals() {
		byId[p.GetId()] = p
	}
	s.Require().Equal("support-bot", byId[first.Id.String()].GetAgentName())
	s.Require().Equal("Test rationale.", byId[first.Id.String()].GetRationale(), "the agent's account comes with the proposal")
	s.Require().NotNil(byId[first.Id.String()].GetProvenance())
	s.Require().Equal(int64(2), pending.GetTotal(), "the total of what waits comes with the list")
	waiting, err := s.props.GetProposal(reviewer, &mandatev1.GetProposalRequest{Id: first.Id.String()})
	s.Require().NoError(err)
	s.Require().Equal("low", waiting.GetProposal().GetApprovalRisk())
	// The action is made high risk: an approval would now run at high.
	_, err = s.actions.DefineAction(owner, &mandatev1.DefineActionRequest{
		Key: "tickets.create", Description: "Open a ticket", Risk: "high",
		SchemaJson: `{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}`,
		Connector:  "webhook", ConnectorUrl: "https://helpdesk.example/hooks",
	})
	s.Require().NoError(err)
	raised, err := s.props.GetProposal(reviewer, &mandatev1.GetProposalRequest{Id: first.Id.String()})
	s.Require().NoError(err)
	s.Require().Equal("low", raised.GetProposal().GetRisk(), "as proposed")
	s.Require().Equal("high", raised.GetProposal().GetApprovalRisk(), "as an approval would run")
	_, err = s.actions.DefineAction(owner, &mandatev1.DefineActionRequest{
		Key: "tickets.create", Description: "Open a ticket", Risk: "low", MayExecute: true,
		SchemaJson: `{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}`,
		Connector:  "webhook", ConnectorUrl: "https://helpdesk.example/hooks",
	})
	s.Require().NoError(err)
	s.Require().Equal("flagged", byId[second.Id.String()].GetSkipReason())
	s.Require().Equal("high", byId[second.Id.String()].GetRisk())

	approved, err := s.props.ApproveProposal(reviewer, &mandatev1.ApproveProposalRequest{Id: first.Id.String(), Title: "Printer jams on floor 3", Note: "ok", PayloadJson: `{"subject":"Printer jams on 3"}`})
	s.Require().NoError(err)
	s.Require().Equal("approved", approved.GetProposal().GetStatus())
	s.Require().True(approved.GetProposal().GetEdited())
	s.Require().Equal("Printer jams on floor 3", approved.GetProposal().GetTitle(), "the approver's title")
	s.Require().JSONEq(`{"subject":"Printer jams on 3"}`, approved.GetProposal().GetPayloadJson(), "the approver's payload")
	s.Require().JSONEq(`{"subject":"Printer jams"}`, approved.GetProposal().GetProposedPayloadJson(), "the agent's payload is kept beside it")
	s.Require().Equal("ok", approved.GetProposal().GetDecisionNote())
	s.Require().Equal("pending", approved.GetProposal().GetDelivery().GetStatus(), "the delivery waits for the job")
	_, err = s.props.ApproveProposal(reviewer, &mandatev1.ApproveProposalRequest{Id: second.Id.String()})
	s.code(err, connect.CodeInvalidArgument)
	rejected, err := s.props.RejectProposal(reviewer, &mandatev1.RejectProposalRequest{Id: second.Id.String(), Note: "steering text"})
	s.Require().NoError(err)
	s.Require().Equal("rejected", rejected.GetProposal().GetStatus())
	_, err = s.props.RejectProposal(reviewer, &mandatev1.RejectProposalRequest{Id: second.Id.String()})
	s.code(err, connect.CodeFailedPrecondition)

	one, err := s.props.GetProposal(owner, &mandatev1.GetProposalRequest{Id: first.Id.String()})
	s.Require().NoError(err)
	s.Require().Len(one.GetHistory(), 2)
	s.Require().Equal("proposal.approved", one.GetHistory()[1].GetType())
	s.Require().Equal("Reviewer", one.GetHistory()[1].GetActorName(), "people are named in the chain view")
	_, err = s.props.GetProposal(owner, &mandatev1.GetProposalRequest{Id: uuid.NewString()})
	s.code(err, connect.CodeNotFound)

	verified, err := s.evidence.VerifyChain(reviewer, &mandatev1.VerifyChainRequest{})
	s.Require().NoError(err)
	s.Require().True(verified.GetOk())
	s.Require().Greater(verified.GetEvents(), int64(8))
	eventsList, err := s.evidence.ListEvents(reviewer, &mandatev1.ListEventsRequest{SubjectId: first.Id.String()})
	s.Require().NoError(err)
	s.Require().Len(eventsList.GetEvents(), 2)
	chain, err := s.evidence.ExportChain(owner, &mandatev1.ExportChainRequest{})
	s.Require().NoError(err)
	s.Require().Equal(int(verified.GetEvents()), strings.Count(chain.GetJsonl(), "\n"))
	decisions, err := s.evidence.ExportDecisions(owner, &mandatev1.ExportDecisionsRequest{})
	s.Require().NoError(err)
	s.Require().Equal(2, strings.Count(decisions.GetJsonl(), "\n"))

	// The pending delivery has no receiver here; a person can only resend a failed one.
	_, err = s.props.RedeliverProposal(reviewer, &mandatev1.RedeliverProposalRequest{Id: first.Id.String()})
	s.code(err, connect.CodeAlreadyExists)

	// An owner removes a person; from then on their session opens nothing
	// in this tenant, and still opens their other tenants.
	elsewhere := s.W.NewTenant(s.T(), "Elsewhere")
	_, err = s.W.Kernel.Tenants.AddPerson(elsewhere.Owner, "second@test.example", "Second", "reviewer")
	s.Require().NoError(err)
	secondToken := s.login("second@test.example")
	secondPerson := as(secondToken, t.Id.String())
	_, err = s.persons.ListPersons(secondPerson, &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
	_, err = s.persons.RemovePerson(reviewer, &mandatev1.RemovePersonRequest{Id: added.GetPerson().GetId()})
	s.code(err, connect.CodePermissionDenied)
	_, err = s.persons.RemovePerson(owner, &mandatev1.RemovePersonRequest{Id: added.GetPerson().GetId()})
	s.Require().NoError(err)
	// The session lives on, but not in this tenant: the next call checks
	// the membership.
	_, err = s.persons.ListPersons(secondPerson, &mandatev1.ListPersonsRequest{})
	s.code(err, connect.CodePermissionDenied)
	_, err = s.persons.ListPersons(as(s.login("second@test.example"), t.Id.String()), &mandatev1.ListPersonsRequest{})
	s.code(err, connect.CodePermissionDenied)
	_, err = s.persons.ListPersons(as(secondToken, elsewhere.Id.String()), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err, "removal from one tenant leaves the session and the other tenants alone")
	me, err := s.auth.Me(as(secondToken, ""), &mandatev1.MeRequest{})
	s.Require().NoError(err)
	s.Require().Len(me.GetTenants(), 1)
	s.Require().Equal(elsewhere.Id.String(), me.GetTenants()[0].GetTenantId())
	people, err := s.persons.ListPersons(owner, &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
	removed := 0
	for _, p := range people.GetPersons() {
		if p.GetRemovedAt() != nil {
			removed++
		}
	}
	s.Require().Equal(1, removed)

	_, err = s.actions.RemoveAction(owner, &mandatev1.RemoveActionRequest{Key: "tickets.create"})
	s.Require().NoError(err)
	_, err = s.mandates.RevokeMandate(owner, &mandatev1.RevokeMandateRequest{Id: granted.GetMandate().GetId()})
	s.Require().NoError(err)
	_, err = s.agents.RevokeToken(owner, &mandatev1.RevokeTokenRequest{Id: issued.GetToken().GetId()})
	s.Require().NoError(err)
	_, err = s.agents.RevokeAgent(owner, &mandatev1.RevokeAgentRequest{Id: agent.GetAgent().GetId()})
	s.Require().NoError(err)
	agentsList, err := s.agents.ListAgents(reviewer, &mandatev1.ListAgentsRequest{})
	s.Require().NoError(err)
	s.Require().NotNil(agentsList.GetAgents()[len(agentsList.GetAgents())-1].GetRevokedAt())
}

// A high-risk proposal is approved over the people API only with the
// explicit confirmation the inbox sends; without it nothing happens.
func (s *ApiSuite) Test_AnUnknownStatusIsRefusedNotAnEmptyQueue() {
	t := s.W.NewTenant(s.T(), "Acme")
	reviewer := as(s.login("reviewer-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	_, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Action: "test.manual", Title: "Waiting", Payload: json.RawMessage(`{}`), IdempotencyKey: "status-1", Rationale: "Why."})
	s.Require().NoError(err)

	_, err = s.props.ListProposals(reviewer, &mandatev1.ListProposalsRequest{Status: "waiting"})
	s.code(err, connect.CodeInvalidArgument)
	s.Require().Contains(err.Error(), "pending, approved, rejected, executed, withdrawn, expired, would_allow, dismissed", "the refusal names the valid statuses")
	_, err = s.W.Kernel.Proposals.Count(t.Owner, "Pending", nil)
	s.Require().Equal(fault.InvalidArgument, fault.CodeOf(err), "the total refuses it as well")

	pending, err := s.props.ListProposals(reviewer, &mandatev1.ListProposalsRequest{Status: "pending"})
	s.Require().NoError(err)
	s.Require().Len(pending.GetProposals(), 1)
	s.Require().Equal(int64(1), pending.GetTotal())
	all, err := s.props.ListProposals(reviewer, &mandatev1.ListProposalsRequest{})
	s.Require().NoError(err, "an empty status means all")
	s.Require().Len(all.GetProposals(), 1)
}

func (s *ApiSuite) Test_AHighRiskApprovalNeedsTheConfirmation() {
	t := s.W.NewTenant(s.T(), "Acme")
	reviewer := as(s.login("reviewer-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	high, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "test.high", Title: "High", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)

	_, err = s.props.ApproveProposal(reviewer, &mandatev1.ApproveProposalRequest{Id: high.Id.String()})
	s.code(err, connect.CodeInvalidArgument)
	still, err := s.props.GetProposal(reviewer, &mandatev1.GetProposalRequest{Id: high.Id.String()})
	s.Require().NoError(err)
	s.Require().Equal("pending", still.GetProposal().GetStatus(), "a refused approval changes nothing")
	s.Require().NotContains(s.W.Applied(), high.Id)

	approved, err := s.props.ApproveProposal(reviewer, &mandatev1.ApproveProposalRequest{Id: high.Id.String(), Confirm: true, Note: "checked"})
	s.Require().NoError(err)
	s.Require().Equal("approved", approved.GetProposal().GetStatus())
	s.Require().Equal("high", approved.GetProposal().GetRisk())
	s.Require().Equal("checked", approved.GetProposal().GetDecisionNote())
	s.Require().Contains(s.W.Applied(), high.Id)
}

// A refusal for a state that has to pass first, or for a schema changed
// since the proposal, says what to do, and the inbox shows that text. The
// code stays already_exists, as 0.1.0 answers it.
func (s *ApiSuite) Test_ARefusalOfTheStateSaysWhatToDo() {
	t := s.W.NewTenant(s.T(), "Acme")
	owner := as(s.login("owner-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	reviewer := as(s.login("reviewer-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"unknown","reason":"the bank did not answer"}`))
	}))
	defer receiver.Close()
	define := func(schema string) {
		s.T().Helper()
		_, err := s.actions.DefineAction(owner, &mandatev1.DefineActionRequest{
			Key: "tickets.create", Description: "Open a ticket", Risk: "low",
			SchemaJson: schema, Connector: "webhook", ConnectorUrl: receiver.URL,
		})
		s.Require().NoError(err)
	}
	define(`{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}`)
	propose := func(subject string) uuid.UUID {
		s.T().Helper()
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "tickets.create", Title: subject, Payload: json.RawMessage(`{"subject":"` + subject + `"}`)})
		s.Require().NoError(err)
		return p.Id
	}
	refund, toner := propose("Refund"), propose("Toner")

	// The receiver does not know whether it was done: settling waits
	// until the work can no longer happen, and the refusal says until when.
	_, err := s.props.ApproveProposal(reviewer, &mandatev1.ApproveProposalRequest{Id: refund.String(), Note: "ok"})
	s.Require().NoError(err)
	delivery, found, err := s.W.Kernel.Deliveries.Get(t.Owner, refund)
	s.Require().NoError(err)
	s.Require().True(found)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(context.Background(), t.Id, delivery.Id, false))
	unknown, err := s.props.GetProposal(reviewer, &mandatev1.GetProposalRequest{Id: refund.String()})
	s.Require().NoError(err)
	s.Require().Equal("unknown", unknown.GetProposal().GetDelivery().GetStatus())
	_, err = s.props.SettleDelivery(reviewer, &mandatev1.SettleDeliveryRequest{Id: refund.String(), Done: false})
	s.code(err, connect.CodeAlreadyExists)
	var refused *connect.Error
	s.Require().ErrorAs(err, &refused)
	s.Require().Contains(refused.Message(), "not yet: the receiver may still be carrying it out until ")
	s.Require().Contains(refused.Message(), delivery.SettleAfter().UTC().Format(time.RFC3339), "the refusal names the moment")

	// The schema now asks for more than the agent sent: the approval is
	// refused with what to change, and the proposal still waits.
	define(`{"type":"object","required":["subject","amount"],"properties":{"subject":{"type":"string"},"amount":{"type":"number"}}}`)
	_, err = s.props.ApproveProposal(reviewer, &mandatev1.ApproveProposalRequest{Id: toner.String(), Note: "ok"})
	s.code(err, connect.CodeAlreadyExists)
	s.Require().ErrorAs(err, &refused)
	s.Require().Contains(refused.Message(), "the action's schema has changed since this was proposed")
	s.Require().Contains(refused.Message(), "edit the payload to fit, or reject the proposal")
	still, err := s.props.GetProposal(reviewer, &mandatev1.GetProposalRequest{Id: toner.String()})
	s.Require().NoError(err)
	s.Require().Equal("pending", still.GetProposal().GetStatus(), "a refused approval changes nothing")
}

// A waiting proposal that a condition stopped says which comparison and
// what the payload had instead.
// A broken chain is an answer, not a failure of the call: it says so and
// where.
func (s *ApiSuite) Test_VerifyChainReportsATamperedEvent() {
	t := s.W.NewTenant(s.T(), "Acme")
	reviewer := as(s.login("reviewer-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	verified, err := s.evidence.VerifyChain(reviewer, &mandatev1.VerifyChainRequest{})
	s.Require().NoError(err)
	s.Require().True(verified.GetOk(), verified.GetError())
	s.Require().Empty(verified.GetError())

	s.Require().NoError(s.W.Pg.Tamper(context.Background(),
		`UPDATE mandate.events SET payload = payload || '{"forged": true}' WHERE tenant_id = $1 AND seq = 2`, t.Id))
	broken, err := s.evidence.VerifyChain(reviewer, &mandatev1.VerifyChainRequest{})
	s.Require().NoError(err)
	s.Require().False(broken.GetOk())
	s.Require().Equal(events.ErrBroken.Error()+" at seq 2", broken.GetError())
	s.Require().Equal(int64(2), broken.GetEvents(), "events read up to the broken one")
}

func (s *ApiSuite) Test_AWaitingProposalSaysWhatTheConditionMissed() {
	t := s.W.NewTenant(s.T(), "Acme")
	reviewer := as(s.login("reviewer-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.low"}, MaxRisk: "low", BudgetCount: 10, PolicyRef: "P", Days: 30, Condition: `payload.amount <= 500`})
	s.Require().NoError(err)
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "test.low", Title: "Big", Payload: json.RawMessage(`{"amount": 750}`)})
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, p.Id))
	got, err := s.props.GetProposal(reviewer, &mandatev1.GetProposalRequest{Id: p.Id.String()})
	s.Require().NoError(err)
	s.Require().Equal(mandates.SkipConditionUnmet, got.GetProposal().GetSkipReason())
	s.Require().Equal([]string{"mandate " + m.Id.String()[:8] + ": payload.amount <= 500 (payload.amount is 750)"}, got.GetProposal().GetUnmetConditions())

	// Another reason to wait says nothing about conditions.
	other, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "test.manual", Title: "Manual", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	got, err = s.props.GetProposal(reviewer, &mandatev1.GetProposalRequest{Id: other.Id.String()})
	s.Require().NoError(err)
	s.Require().Empty(got.GetProposal().GetUnmetConditions())
}

func (s *ApiSuite) Test_OwnersSetNotificationsAndTheAddressStaysSecret() {
	t := s.W.NewTenant(s.T(), "Acme")
	owner := as(s.login("owner-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	reviewer := as(s.login("reviewer-"+t.Id.String()[:8]+"@test.example"), t.Id.String())
	none, err := s.notifiers.GetNotifier(reviewer, &mandatev1.GetNotifierRequest{})
	s.Require().NoError(err)
	s.Require().Nil(none.GetNotifier())
	_, err = s.notifiers.SetNotifier(reviewer, &mandatev1.SetNotifierRequest{Kind: "slack", Url: "https://hooks.slack.com/services/T/B/x"})
	s.code(err, connect.CodePermissionDenied)
	_, err = s.notifiers.SetNotifier(owner, &mandatev1.SetNotifierRequest{Kind: "slack", Url: "https://example.com/x"})
	s.code(err, connect.CodeInvalidArgument)
	set, err := s.notifiers.SetNotifier(owner, &mandatev1.SetNotifierRequest{Kind: "slack", Url: "https://hooks.slack.com/services/T/B/x"})
	s.Require().NoError(err)
	s.Require().Equal("slack", set.GetNotifier().GetKind())
	read, err := s.notifiers.GetNotifier(reviewer, &mandatev1.GetNotifierRequest{})
	s.Require().NoError(err)
	s.Require().Equal(set.GetNotifier().GetId(), read.GetNotifier().GetId())
	s.Require().NotContains(read.String(), "hooks.slack.com", "the address is never returned")
	_, err = s.notifiers.RemoveNotifier(reviewer, &mandatev1.RemoveNotifierRequest{})
	s.code(err, connect.CodePermissionDenied)
	_, err = s.notifiers.RemoveNotifier(owner, &mandatev1.RemoveNotifierRequest{})
	s.Require().NoError(err)
}

func (s *ApiSuite) Test_OidcSignsIn() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "oidc@acme.example", "Olga", "reviewer")
	s.Require().NoError(err)
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	start, err := browser.Get(s.server.URL + auth.PathStart)
	s.Require().NoError(err)
	_ = start.Body.Close()
	s.Require().Equal(http.StatusFound, start.StatusCode)
	location, err := url.Parse(start.Header.Get("Location"))
	s.Require().NoError(err)
	s.Require().True(strings.HasPrefix(location.String(), s.provider.server.URL+"/authorize"))
	s.Require().Equal("S256", location.Query().Get("code_challenge_method"))
	s.Require().NotEmpty(location.Query().Get("code_challenge"))
	state := location.Query().Get("state")
	s.Require().NotEmpty(state)

	wrong, err := browser.Get(s.server.URL + auth.PathCallback + "?code=abc&state=other")
	s.Require().NoError(err)
	_ = wrong.Body.Close()
	s.Require().Equal(http.StatusBadRequest, wrong.StatusCode)

	// The provider's cookie is gone after a mismatch; start again.
	start, err = browser.Get(s.server.URL + auth.PathStart)
	s.Require().NoError(err)
	_ = start.Body.Close()
	location, _ = url.Parse(start.Header.Get("Location"))
	state = location.Query().Get("state")
	s.provider.authorize(location)
	s.provider.email = "oidc@acme.example"
	done, err := browser.Get(s.server.URL + auth.PathCallback + "?code=good&state=" + url.QueryEscape(state))
	s.Require().NoError(err)
	_ = done.Body.Close()
	s.Require().Equal(http.StatusFound, done.StatusCode)
	s.Require().Equal("/", done.Header.Get("Location"), "the exchange's verifier matched the challenge in the authorize URL")

	withCookie := mandatev1connect.NewAuthServiceClient(&http.Client{Jar: jar}, s.server.URL)
	me, err := withCookie.Me(context.Background(), &mandatev1.MeRequest{})
	s.Require().NoError(err)
	s.Require().Equal("oidc@acme.example", me.GetEmail())
	s.Require().Equal("Olga", me.GetName())
	s.Require().Len(me.GetTenants(), 1)
	s.Require().Equal("reviewer", me.GetTenants()[0].GetRole())

	// An unverified email is refused: it could be anyone's.
	s.provider.verified = false
	start, err = browser.Get(s.server.URL + auth.PathStart)
	s.Require().NoError(err)
	_ = start.Body.Close()
	location, _ = url.Parse(start.Header.Get("Location"))
	s.provider.authorize(location)
	unverified, err := browser.Get(s.server.URL + auth.PathCallback + "?code=good&state=" + url.QueryEscape(location.Query().Get("state")))
	s.Require().NoError(err)
	_ = unverified.Body.Close()
	s.Require().Equal(http.StatusUnauthorized, unverified.StatusCode)
	s.provider.verified = true

	// An id token for another client is refused.
	s.provider.audience = "someone-else"
	start, err = browser.Get(s.server.URL + auth.PathStart)
	s.Require().NoError(err)
	_ = start.Body.Close()
	location, _ = url.Parse(start.Header.Get("Location"))
	s.provider.authorize(location)
	bad, err := browser.Get(s.server.URL + auth.PathCallback + "?code=good&state=" + url.QueryEscape(location.Query().Get("state")))
	s.Require().NoError(err)
	_ = bad.Body.Close()
	s.Require().Equal(http.StatusUnauthorized, bad.StatusCode)
	s.provider.audience = "mandate-web"
}

// fakeProvider is an OpenID provider for tests: discovery, JWKS and a
// token endpoint that issues a signed id token for whoever the test says.
// Like a real provider it enforces PKCE: the token endpoint refuses a
// code_verifier that does not hash to the challenge of the authorize step.
type fakeProvider struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	email    string
	subject  string
	audience string
	verified bool
	// challenge and challengeMethod are what the last authorize URL
	// carried; authorize records them, and each exchange uses them up.
	challenge       string
	challengeMethod string
}

// authorize stands in for the provider's /authorize step, which the
// test's browser does not follow: it remembers the PKCE challenge of the
// authorize URL that pgmandate redirected to.
func (p *fakeProvider) authorize(location *url.URL) {
	p.challenge = location.Query().Get("code_challenge")
	p.challengeMethod = location.Query().Get("code_challenge_method")
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{key: key, email: "oidc@acme.example", subject: "user-1", audience: "mandate-web", verified: true}
	mux := http.NewServeMux()
	p.server = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token",
			"jwks_uri": p.server.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostFormValue("code") != "good" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		challenge, method := p.challenge, p.challengeMethod
		p.challenge, p.challengeMethod = "", ""
		digest := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if method != "S256" || challenge == "" || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		now := time.Now().Unix()
		claims := map[string]any{"iss": p.server.URL, "sub": p.subject, "aud": p.audience, "exp": now + 300, "iat": now, "email": p.email, "email_verified": p.verified, "name": "Olga"}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 300, "id_token": p.jwt(claims)})
	})
	return p
}

func (p *fakeProvider) jwt(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	payload, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	signed, _ := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(signed)
}

// signIn goes through the provider as the identity, and returns a browser
// with the session's cookie.
func (s *ApiSuite) signIn(email, subject string) *http.Client {
	s.T().Helper()
	s.provider.email, s.provider.subject = email, subject
	defer func() { s.provider.email, s.provider.subject = "oidc@acme.example", "user-1" }()
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start, err := browser.Get(s.server.URL + auth.PathStart)
	s.Require().NoError(err)
	_ = start.Body.Close()
	location, err := url.Parse(start.Header.Get("Location"))
	s.Require().NoError(err)
	s.provider.authorize(location)
	done, err := browser.Get(s.server.URL + auth.PathCallback + "?code=good&state=" + url.QueryEscape(location.Query().Get("state")))
	s.Require().NoError(err)
	_ = done.Body.Close()
	s.Require().Equal(http.StatusFound, done.StatusCode)
	return &http.Client{Jar: jar}
}

// A person is bound to the identity that first signs in as them, in each
// tenant; another identity with the same email is refused there, and an
// email that changes at the provider still reaches the person.
func (s *ApiSuite) Test_APersonIsBoundToOneIdentity() {
	t := s.W.NewTenant(s.T(), "Acme")
	other := s.W.NewTenant(s.T(), "Other")
	ada, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "ada@acme.example", "Ada", "reviewer")
	s.Require().NoError(err)
	_, err = s.W.Kernel.Tenants.AddPerson(other.Owner, "ada@acme.example", "Ada", "reviewer")
	s.Require().NoError(err)
	in := func(tenant testfix.Tenant) context.Context { return as("", tenant.Id.String()) }

	first := s.signIn("ada@acme.example", "ada-1")
	people := mandatev1connect.NewPersonServiceClient(first, s.server.URL)
	_, err = people.ListPersons(in(t), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err, "the first call binds")
	s.Require().Contains(s.eventTypes(t.Owner, ada.Id), "person.bound")
	var issuer string
	var subjectDigest string
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(),
		`SELECT payload->>'issuer', payload->>'subjectSha256' FROM mandate.events WHERE type = 'person.bound' AND subject_id = $1`, ada.Id).Scan(&issuer, &subjectDigest))
	s.Require().Equal(s.provider.server.URL, issuer)
	s.Require().Len(subjectDigest, 64, "the subject is in the log as a digest")

	// Another identity with Ada's email: no tenant where she is bound, and
	// a call there is refused, with what to do.
	impostor := s.signIn("ada@acme.example", "someone-else")
	me, err := mandatev1connect.NewAuthServiceClient(impostor, s.server.URL).Me(context.Background(), &mandatev1.MeRequest{})
	s.Require().NoError(err)
	var names []string
	for _, m := range me.GetTenants() {
		names = append(names, m.GetTenantName())
	}
	s.Require().Equal([]string{"Other"}, names, "only where Ada is still unbound")
	_, err = mandatev1connect.NewPersonServiceClient(impostor, s.server.URL).ListPersons(in(t), &mandatev1.ListPersonsRequest{})
	s.code(err, connect.CodePermissionDenied)
	s.Require().Contains(err.Error(), "belongs to another sign-in in this tenant")
	// In the other tenant Ada was still unbound, so this identity binds
	// there: a binding is per tenant.
	_, err = mandatev1connect.NewPersonServiceClient(impostor, s.server.URL).ListPersons(in(other), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
	_, err = mandatev1connect.NewPersonServiceClient(first, s.server.URL).ListPersons(in(other), &mandatev1.ListPersonsRequest{})
	s.code(err, connect.CodePermissionDenied)

	// Ada's address changes at the provider: she is still Ada, by her
	// identity.
	renamed := s.signIn("ada.lovelace@acme.example", "ada-1")
	listed, err := mandatev1connect.NewPersonServiceClient(renamed, s.server.URL).ListPersons(in(t), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
	s.Require().NotEmpty(listed.GetPersons())

	// Dev login reaches people by email, bound or not, and binds nobody.
	dev := as(s.login("ada@acme.example"), t.Id.String())
	_, err = s.persons.ListPersons(dev, &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)

	// An owner removes Ada and adds her again: the new row is unbound, and
	// the next identity binds it.
	_, err = s.W.Kernel.Tenants.AddPerson(t.Owner, "grace@acme.example", "Grace", "owner")
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.Tenants.RemovePerson(t.Owner, ada.Id))
	again, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "ada@acme.example", "Ada", "reviewer")
	s.Require().NoError(err)
	_, err = mandatev1connect.NewPersonServiceClient(impostor, s.server.URL).ListPersons(in(t), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err, "re-added, the person binds to whoever signs in first")
	s.Require().Contains(s.eventTypes(t.Owner, again.Id), "person.bound")
}

// eventTypes lists the types of the events about a subject, in order.
func (s *ApiSuite) eventTypes(ctx context.Context, subject uuid.UUID) []string {
	s.T().Helper()
	var types []string
	s.Require().NoError(db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		list, err := events.List(ctx, tx, &subject, 0)
		for _, e := range list {
			types = append(types, e.Type)
		}
		return err
	}))
	return types
}

// Two first calls at once bind the person once.
func (s *ApiSuite) Test_TwoFirstCallsBindOnce() {
	t := s.W.NewTenant(s.T(), "Acme")
	bo, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "bo@acme.example", "Bo", "reviewer")
	s.Require().NoError(err)
	browser := s.signIn("bo@acme.example", "bo-1")
	people := mandatev1connect.NewPersonServiceClient(browser, s.server.URL)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = people.ListPersons(as("", t.Id.String()), &mandatev1.ListPersonsRequest{})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		s.Require().NoError(err)
	}
	bound := 0
	for _, e := range s.eventTypes(t.Owner, bo.Id) {
		if e == "person.bound" {
			bound++
		}
	}
	s.Require().Equal(1, bound)
}

// A session cookie that opens nothing any more is cleared, so a page left
// open stops sending it; a bearer token is not a cookie to clear.
func (s *ApiSuite) Test_AStaleSessionCookieIsCleared() {
	ask := func(set func(*http.Request)) (int, []*http.Cookie) {
		req, err := http.NewRequest(http.MethodPost, s.server.URL+"/mandate.v1.AuthService/Me", strings.NewReader(`{}`))
		s.Require().NoError(err)
		req.Header.Set("Content-Type", "application/json")
		set(req)
		resp, err := http.DefaultClient.Do(req)
		s.Require().NoError(err)
		defer resp.Body.Close() //nolint:errcheck
		return resp.StatusCode, resp.Cookies()
	}
	stale := strings.Repeat("x", 43)
	status, cookies := ask(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: stale}) })
	s.Require().Equal(http.StatusUnauthorized, status)
	var cleared bool
	for _, c := range cookies {
		if c.Name == auth.CookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	s.Require().True(cleared, "the cookie is cleared")
	status, cookies = ask(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+stale) })
	s.Require().Equal(http.StatusUnauthorized, status)
	s.Require().Empty(cookies)
}

// Dev login reaches a person by email and binds nobody.
func (s *ApiSuite) Test_DevLoginBindsNobody() {
	t := s.W.NewTenant(s.T(), "Acme")
	cy, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "cy@acme.example", "Cy", "reviewer")
	s.Require().NoError(err)
	_, err = s.persons.ListPersons(as(s.login("cy@acme.example"), t.Id.String()), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
	s.Require().NotContains(s.eventTypes(t.Owner, cy.Id), "person.bound")
	var issuer *string
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(), `SELECT issuer FROM mandate.persons WHERE id = $1`, cy.Id).Scan(&issuer))
	s.Require().Nil(issuer)
	// A provider's sign-in then still binds Cy.
	_, err = mandatev1connect.NewPersonServiceClient(s.signIn("cy@acme.example", "cy-1"), s.server.URL).ListPersons(as("", t.Id.String()), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
	s.Require().Contains(s.eventTypes(t.Owner, cy.Id), "person.bound")
}

// A dev login session opens nothing on a server where dev login is off,
// though both share the database; a provider's session still does.
func (s *ApiSuite) Test_ADevLoginSessionOpensNothingWhereDevLoginIsOff() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "di@acme.example", "Di", "reviewer")
	s.Require().NoError(err)
	devToken := s.login("di@acme.example")
	strict := auth.NewService(s.W.Pg.App, auth.Config{
		SessionTtl: time.Hour, PublicUrl: "http://127.0.0.1",
		Oidc: &auth.OidcConfig{Issuer: s.provider.server.URL, ClientId: "mandate-web", ClientSecret: "shh"},
	})
	mux := http.NewServeMux()
	api.New(s.W.Kernel, strict).Mount(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	_, err = strict.Authenticate(context.Background(), devToken)
	s.Require().Error(err)
	s.Require().Equal(fault.Unauthenticated, fault.CodeOf(err))
	_, err = mandatev1connect.NewAuthServiceClient(http.DefaultClient, server.URL).Me(as(devToken, ""), &mandatev1.MeRequest{})
	s.code(err, connect.CodeUnauthenticated)
	_, err = mandatev1connect.NewPersonServiceClient(http.DefaultClient, server.URL).ListPersons(as(devToken, t.Id.String()), &mandatev1.ListPersonsRequest{})
	s.code(err, connect.CodeUnauthenticated)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/mandate.v1.AuthService/Me", strings.NewReader(`{}`))
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: devToken})
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusUnauthorized, resp.StatusCode)
	var cleared bool
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	s.Require().True(cleared, "the cookie is cleared")

	// Where dev login is on, the same session still works.
	_, err = s.persons.ListPersons(as(devToken, t.Id.String()), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
	// A provider's session opens the strict server, which trusts the same
	// provider.
	oidcToken := s.sessionToken(s.signIn("di@acme.example", "di-1"))
	_, err = mandatev1connect.NewPersonServiceClient(http.DefaultClient, server.URL).ListPersons(as(oidcToken, t.Id.String()), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
}

// sessionToken is the session token in a browser's cookie for the server.
func (s *ApiSuite) sessionToken(browser *http.Client) string {
	s.T().Helper()
	serverUrl, err := url.Parse(s.server.URL)
	s.Require().NoError(err)
	for _, c := range browser.Jar.Cookies(serverUrl) {
		if c.Name == auth.CookieName {
			return c.Value
		}
	}
	s.Require().Fail("the browser has no session cookie")
	return ""
}

// A provider's session opens nothing on a server that no longer trusts that
// provider, removed or replaced, and binds nobody there; the person is then
// still free for whoever signs in through the provider that is configured.
func (s *ApiSuite) Test_ASessionFromAProviderNoLongerConfiguredOpensNothing() {
	t := s.W.NewTenant(s.T(), "Acme")
	eve, err := s.W.Kernel.Tenants.AddPerson(t.Owner, "eve@acme.example", "Eve", "reviewer")
	s.Require().NoError(err)
	token := s.sessionToken(s.signIn("eve@acme.example", "eve-1"))

	servers := map[string]*auth.Service{
		"no provider": auth.NewService(s.W.Pg.App, auth.Config{SessionTtl: time.Hour, PublicUrl: "http://127.0.0.1"}),
		"another provider": auth.NewService(s.W.Pg.App, auth.Config{
			SessionTtl: time.Hour, PublicUrl: "http://127.0.0.1",
			Oidc: &auth.OidcConfig{Issuer: "https://new-provider.example", ClientId: "mandate-web", ClientSecret: "shh"},
		}),
	}
	for name, service := range servers {
		_, err = service.Authenticate(context.Background(), token)
		s.Require().Error(err, name)
		s.Require().Equal(fault.Unauthenticated, fault.CodeOf(err), name)
		mux := http.NewServeMux()
		api.New(s.W.Kernel, service).Mount(mux)
		server := httptest.NewServer(mux)
		_, meErr := mandatev1connect.NewAuthServiceClient(http.DefaultClient, server.URL).Me(as(token, ""), &mandatev1.MeRequest{})
		_, listErr := mandatev1connect.NewPersonServiceClient(http.DefaultClient, server.URL).ListPersons(as(token, t.Id.String()), &mandatev1.ListPersonsRequest{})
		server.Close()
		s.code(meErr, connect.CodeUnauthenticated)
		s.code(listErr, connect.CodeUnauthenticated)
	}
	s.Require().NotContains(s.eventTypes(t.Owner, eve.Id), "person.bound", "a refused session binds nobody")
	var issuer *string
	s.Require().NoError(s.W.Pg.Owner.QueryRow(context.Background(), `SELECT issuer FROM mandate.persons WHERE id = $1`, eve.Id).Scan(&issuer))
	s.Require().Nil(issuer)

	// Where the provider is still the configured one, the session works.
	_, err = s.persons.ListPersons(as(token, t.Id.String()), &mandatev1.ListPersonsRequest{})
	s.Require().NoError(err)
	s.Require().Contains(s.eventTypes(t.Owner, eve.Id), "person.bound")
}
