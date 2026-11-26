package agentapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/suite"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/agent/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/agent/v1/agentv1connect"
	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/mcp"
	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/agentapi"
	"github.com/andreas-lindfalk/pgmandate/modules/auth"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
)

type AgentApiSuite struct {
	IntegrationSuite
}

func TestAgentApiSuite(t *testing.T) {
	suite.Run(t, new(AgentApiSuite))
}

// thirtyDigits is a number no float64 holds: it must arrive and come back
// with every digit.
const thirtyDigits = "123456789012345678901234567890"

type bearer struct {
	token  string
	cookie *http.Cookie
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// client is a Connect client of the contract, in JSON, as the agent with
// the token.
func (s *AgentApiSuite) client(token string) agentv1connect.AgentServiceClient {
	return agentv1connect.NewAgentServiceClient(&http.Client{Transport: bearer{token: token}}, s.Http.URL, connect.WithProtoJSON())
}

// post calls a method as plain JSON over HTTP, as any language would, and
// returns the status and the body.
func (s *AgentApiSuite) post(transport http.RoundTripper, method, body string) (int, []byte) {
	s.T().Helper()
	req, err := http.NewRequest(http.MethodPost, s.Http.URL+"/mandate.agent.v1.AgentService/"+method, strings.NewReader(body))
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: transport}).Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, out
}

// mcpCall runs a tool over MCP and returns its structured answer.
func (s *AgentApiSuite) mcpCall(token, tool string, arguments any) string {
	s.T().Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test agent", Version: "2.1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: s.Mcp.URL, HTTPClient: &http.Client{Transport: bearer{token: token}}, MaxRetries: -1,
	}, nil)
	s.Require().NoError(err)
	defer session.Close() //nolint:errcheck
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: arguments})
	s.Require().NoError(err)
	s.Require().False(result.IsError, "%v", result.Content)
	return result.Content[0].(*sdk.TextContent).Text
}

// The same service answers over HTTP and over MCP, with the same answer:
// over MCP the JSON text fields are objects, with every digit kept.
func (s *AgentApiSuite) Test_TheSameAnswerOverMcpAndHttp() {
	t := s.W.NewTenant(s.T(), "Acme")
	status, body := s.post(bearer{token: t.Token}, "Propose", `{"action":"test.manual","title":"Big number","idempotencyKey":"run-1:1",
		"rationale":"It needs every digit.","payloadJson":"{\"account\":`+thirtyDigits+`}","runId":"run-1",
		"provenance":{"client":"curl script","clientVersion":"8.1","model":"m-1"}}`)
	s.Require().Equal(http.StatusOK, status, string(body))
	var proposed agentv1.ProposeResponse
	s.Require().NoError(protojson.Unmarshal(body, &proposed))
	s.Require().True(proposed.GetCreated())
	s.Require().Equal(`{"account": `+thirtyDigits+`}`, proposed.GetProposal().GetPayloadJson(), "the payload as JSON text, every digit kept")
	s.Require().Equal(proposals.StatusPending, proposed.GetProposal().GetStatus())
	s.Require().Contains(proposed.GetProposal().GetOutcome(), "waits for a person")
	id := proposed.GetProposal().GetProposalId()

	stored, err := s.W.Kernel.Proposals.Get(t.Owner, uuid.MustParse(id))
	s.Require().NoError(err)
	s.Require().Equal("curl-script", stored.Provenance.Client, "over HTTP the agent names its client itself")
	s.Require().Equal("8.1", stored.Provenance.ClientVersion)

	status, body = s.post(bearer{token: t.Token}, "GetProposal", `{"proposalId":"`+id+`"}`)
	s.Require().Equal(http.StatusOK, status, string(body))
	var overHttp agentv1.GetProposalResponse
	s.Require().NoError(protojson.Unmarshal(body, &overHttp))
	asMcp, err := mcp.Structured(&overHttp)
	s.Require().NoError(err)
	overMcp := s.mcpCall(t.Token, "get_proposal", map[string]any{"proposal_id": id})
	s.Require().JSONEq(string(asMcp), overMcp, "one answer, two doors")
	s.Require().Contains(overMcp, `"payload":{"account":`+thirtyDigits+`}`, "over MCP the payload is an object, with every digit")
	s.Require().NotContains(overMcp, "payloadJson")

	// The same request over MCP is a repeat of the one over HTTP.
	repeat := s.mcpCall(t.Token, "propose", json.RawMessage(`{"action":"test.manual","title":"Big number","idempotency_key":"run-1:1",
		"rationale":"It needs every digit.","payload":{"account":`+thirtyDigits+`},"run_id":"run-1"}`))
	var answer map[string]any
	s.Require().NoError(json.Unmarshal([]byte(repeat), &answer))
	s.Require().Equal(false, answer["created"], "created is there when false")
	s.Require().Equal(true, answer["repeat"])
	s.Require().Equal(id, answer["proposal"].(map[string]any)["proposalId"])

	// Both doors are in the log, call by call.
	var vias []string
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		logged, err := events.List(t.Owner, tx, &t.AgentId, 0)
		for _, e := range logged {
			var payload struct {
				Tool      string         `json:"tool"`
				Arguments map[string]any `json:"arguments"`
			}
			if e.Type == "agent.tool_called" && json.Unmarshal(e.Payload, &payload) == nil {
				vias = append(vias, payload.Tool+" "+payload.Arguments["via"].(string))
			}
		}
		return err
	}))
	s.Require().Equal([]string{"propose http", "get_proposal http", "get_proposal mcp", "propose mcp"}, vias)
}

// ListActions over HTTP: the actions, their numbers as numbers, the limits.
func (s *AgentApiSuite) Test_ListActionsOverHttp() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.low"}, MaxRisk: mandates.RiskLow, BudgetCount: 7, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	status, body := s.post(bearer{token: t.Token}, "ListActions", `{}`)
	s.Require().Equal(http.StatusOK, status, string(body))
	var listed agentv1.ListActionsResponse
	s.Require().NoError(protojson.Unmarshal(body, &listed))
	s.Require().Regexp(`^run-[0-9a-f-]{36}$`, listed.GetSuggestedRunId())
	s.Require().Positive(listed.GetLimits().GetProposalsPerMinute())
	var low *agentv1.Action
	for _, a := range listed.GetActions() {
		if a.GetKey() == "test.low" {
			low = a
		}
	}
	s.Require().NotNil(low)
	s.Require().Len(low.GetMandates(), 1)
	s.Require().Equal(int32(7), low.GetMandates()[0].GetRemaining())
	s.Require().Contains(string(body), `"waitSeconds":604800`, "counts are JSON numbers, not strings")

	// Zero is said, not left out: the budget used up, and the execution
	// that used it, which came when none had been spent.
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Action: "test.low", Title: "T", Payload: json.RawMessage(`{}`), IdempotencyKey: "k-1", Rationale: "Why."})
	s.Require().NoError(err)
	for range 6 {
		q, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Action: "test.low", Title: "T", Payload: json.RawMessage(`{}`), IdempotencyKey: uuid.NewString(), Rationale: "Why."})
		s.Require().NoError(err)
		s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, q.Id))
	}
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, p.Id))
	_, body = s.post(bearer{token: t.Token}, "ListActions", `{}`)
	s.Require().Contains(string(body), `"remaining":0`, "a spent budget says 0 left")
	status, body = s.post(bearer{token: t.Token}, "ListProposals", `{"status":"executed","limit":100}`)
	s.Require().Equal(http.StatusOK, status, string(body))
	s.Require().Contains(string(body), `"spent":0`, "the first execution's basis says none was spent before it")
}

// Only an agent's bearer token opens the contract: no token, a bad one, a
// revoked one or a person's session is unauthenticated, and is not logged.
func (s *AgentApiSuite) Test_OnlyAnAgentTokenOpensIt() {
	t := s.W.NewTenant(s.T(), "Acme")
	authService := auth.NewService(s.W.Pg.App, auth.Config{DevLogin: true, SessionTtl: time.Hour, PublicUrl: "http://127.0.0.1"})
	session, err := authService.DevLogin(context.Background(), "owner-"+t.Id.String()[:8]+"@test.example", "Owner")
	s.Require().NoError(err)
	_, revoked, err := s.W.Kernel.Agents.IssueToken(t.Owner, t.AgentId, 1)
	s.Require().NoError(err)
	grant, err := s.W.Kernel.Agents.Authenticate(context.Background(), revoked)
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.Agents.RevokeToken(t.Owner, grant.TokenId))

	for name, transport := range map[string]bearer{
		"no token":                 {},
		"not a token":              {token: "mnd_nothing"},
		"a revoked token":          {token: revoked},
		"a person's bearer":        {token: session},
		"a person's cookie":        {cookie: auth.SessionCookie(session, time.Hour, false)},
		"a cookie and a bad token": {token: "nope", cookie: auth.SessionCookie(session, time.Hour, false)},
	} {
		status, body := s.post(transport, "ListActions", `{}`)
		s.Require().Equal(http.StatusUnauthorized, status, name)
		s.Require().Contains(string(body), `"code":"unauthenticated"`, name)
	}
	s.Require().Empty(s.toolCalls(t), "a call that is not an agent's is not in the log")

	// The scheme's case does not matter, as over MCP.
	req, err := http.NewRequest(http.MethodPost, s.Http.URL+"/mandate.agent.v1.AgentService/ListActions", strings.NewReader(`{}`))
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer "+t.Token)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	s.Require().NoError(resp.Body.Close())
	s.Require().Equal(http.StatusOK, resp.StatusCode)
}

// A caller that goes away during a wait is still in the log, and is told
// canceled, not internal.
func (s *AgentApiSuite) Test_ACallerThatLeavesIsStillRecorded() {
	t := s.W.NewTenant(s.T(), "Acme")
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Action: "test.manual", Title: "T", Payload: json.RawMessage(`{}`), IdempotencyKey: "k-1", Rationale: "Why."})
	s.Require().NoError(err)
	call := agentapi.WithCall(context.Background(), agentapi.Call{Grant: t.Grant, Via: agentapi.ViaHttp})
	wait := &agentv1.GetProposalRequest{ProposalId: p.Id.String(), WaitSeconds: 30}

	gone, cancel := context.WithCancel(call)
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err = s.W.Kernel.AgentApi.GetProposal(gone, wait)
	s.Require().Equal(connect.CodeCanceled, connect.CodeOf(err))

	late, stop := context.WithTimeout(call, 300*time.Millisecond)
	defer stop()
	_, err = s.W.Kernel.AgentApi.GetProposal(late, wait)
	s.Require().Equal(connect.CodeDeadlineExceeded, connect.CodeOf(err))

	calls := s.toolCalls(t)
	s.Require().Len(calls, 2, "both calls are recorded although their callers left")
	for _, c := range calls {
		s.Require().Contains(string(c.Payload), `"failed": true`)
	}
}

// An agent reads and withdraws only its own proposals, through either door.
func (s *AgentApiSuite) Test_AnAgentSeesOnlyItsOwn() {
	t := s.W.NewTenant(s.T(), "Acme")
	mine, err := s.client(t.Token).Propose(context.Background(), &agentv1.ProposeRequest{
		Action: "test.manual", Title: "Mine", PayloadJson: `{}`, IdempotencyKey: "k-1", Rationale: "Mine.",
	})
	s.Require().NoError(err)
	id := mine.GetProposal().GetProposalId()

	other, err := s.W.Kernel.Agents.Create(t.Owner, "other", "", "live")
	s.Require().NoError(err)
	_, otherToken, err := s.W.Kernel.Agents.IssueToken(t.Owner, other.Id, 1)
	s.Require().NoError(err)
	theirs := s.client(otherToken)
	_, err = theirs.GetProposal(context.Background(), &agentv1.GetProposalRequest{ProposalId: id})
	s.Require().Equal(connect.CodeNotFound, connect.CodeOf(err))
	_, err = theirs.WithdrawProposal(context.Background(), &agentv1.WithdrawProposalRequest{ProposalId: id})
	s.Require().Equal(connect.CodeNotFound, connect.CodeOf(err))
	listed, err := theirs.ListProposals(context.Background(), &agentv1.ListProposalsRequest{})
	s.Require().NoError(err)
	s.Require().Empty(listed.GetProposals())
	listed, err = s.client(t.Token).ListProposals(context.Background(), &agentv1.ListProposalsRequest{})
	s.Require().NoError(err)
	s.Require().Len(listed.GetProposals(), 1)
}

// A limit past the contract's 100 gives 100, not the default 20: an agent
// that asks for more is not handed fewer and left to take them as all.
func (s *AgentApiSuite) Test_AListLimitPastTheMostGivesTheMost() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, 200, 200)
	s.Require().NoError(err)
	for range 101 {
		_, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Action: "test.manual", Title: "T", Payload: json.RawMessage(`{}`), IdempotencyKey: uuid.NewString(), Rationale: "Why."})
		s.Require().NoError(err)
	}
	client := s.client(t.Token)
	for limit, want := range map[int32]int{0: 20, 1: 1, 100: 100, 101: 100, 500: 100} {
		listed, err := client.ListProposals(context.Background(), &agentv1.ListProposalsRequest{Limit: limit})
		s.Require().NoError(err)
		s.Require().Equal(want, len(listed.GetProposals()), "limit %d", limit)
	}
	var listed struct {
		Proposals []json.RawMessage `json:"proposals"`
	}
	s.Require().NoError(json.Unmarshal([]byte(s.mcpCall(t.Token, "list_proposals", map[string]any{"limit": 1000})), &listed))
	s.Require().Equal(100, len(listed.Proposals), "over MCP too")
}

// A list says why each waiting proposal waits, as a read of the one
// proposal does: a reason an execution recorded, and one that only the
// action shows, for a run that resumes from list_proposals.
func (s *AgentApiSuite) Test_AListSaysWhyAProposalWaits() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"test.low"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30, Condition: `payload.amount <= 500`})
	s.Require().NoError(err)
	_, err = s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "notes.add", Risk: mandates.RiskLow, MayExecute: true, Connector: actions.ConnectorWebhook, Url: "https://connector.example/hooks",
		Schema: json.RawMessage(`{"type":"object"}`),
	})
	s.Require().NoError(err)
	propose := func(action, payload string) uuid.UUID {
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Action: action, Title: "T", Payload: json.RawMessage(payload), IdempotencyKey: uuid.NewString(), Rationale: "Why."})
		s.Require().NoError(err)
		return p.Id
	}
	unmet := propose("test.low", `{}`)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, unmet))
	removed := propose("notes.add", `{}`)
	s.Require().NoError(s.W.Kernel.Actions.Remove(t.Owner, "notes.add"))
	// Two of one action, so that a looked-up action serves both.
	manual := []uuid.UUID{propose("test.manual", `{}`), propose("test.manual", `{}`)}
	want := map[uuid.UUID]string{unmet: mandates.SkipConditionUnmet, removed: proposals.SkipActionRemoved, manual[0]: proposals.SkipNotExecutable, manual[1]: proposals.SkipNotExecutable}

	client := s.client(t.Token)
	listed, err := client.ListProposals(context.Background(), &agentv1.ListProposalsRequest{})
	s.Require().NoError(err)
	s.Require().Len(listed.GetProposals(), len(want))
	for _, inList := range listed.GetProposals() {
		id := uuid.MustParse(inList.GetProposalId())
		read, err := client.GetProposal(context.Background(), &agentv1.GetProposalRequest{ProposalId: id.String()})
		s.Require().NoError(err)
		one := read.GetProposal()
		s.Require().Equal(want[id], one.GetSkipped(), inList.GetAction())
		s.Require().Equal(one.GetSkipped(), inList.GetSkipped(), inList.GetAction())
		s.Require().Equal(one.GetOutcome(), inList.GetOutcome(), inList.GetAction())
		s.Require().Equal(one.GetNext(), inList.GetNext(), inList.GetAction())
		s.Require().NotEqual("waits for a person", inList.GetOutcome(), "the outcome says why")
		if id == unmet {
			s.Require().NotEmpty(one.GetUnmet(), "a read says what stands in the way")
			s.Require().Empty(inList.GetUnmet(), "a list leaves that to get_proposal")
		}
	}
}

// Each failure has the contract's code, and a rate limit says how long to
// wait.
func (s *AgentApiSuite) Test_ErrorsHaveTheContractsCodes() {
	t := s.W.NewTenant(s.T(), "Acme")
	c := s.client(t.Token)
	ctx := context.Background()
	propose := func(key, payload string) (*agentv1.ProposeResponse, error) {
		return c.Propose(ctx, &agentv1.ProposeRequest{Action: "test.manual", Title: "T", PayloadJson: payload, IdempotencyKey: key, Rationale: "Why."})
	}
	_, err := c.Propose(ctx, &agentv1.ProposeRequest{Action: "Not A Key", Title: "T", PayloadJson: `{}`, IdempotencyKey: "k", Rationale: "Why."})
	s.Require().Equal(connect.CodeInvalidArgument, connect.CodeOf(err))
	// A refused value is never echoed, but the refusal says what a valid
	// one looks like, so the agent can correct itself.
	s.Require().Contains(err.Error(), "invalid action key (9 characters): lowercase words of letters, digits and _")
	s.Require().Contains(err.Error(), "such as tickets.create")
	// A well-formed key longer than an action key may be is refused by the
	// contract, with the same form, not later as a bare invalid key.
	_, err = c.Propose(ctx, &agentv1.ProposeRequest{Action: "test." + strings.Repeat("a", 145), Title: "T", PayloadJson: `{}`, IdempotencyKey: "k", Rationale: "Why."})
	s.Require().Equal(connect.CodeInvalidArgument, connect.CodeOf(err))
	s.Require().Contains(err.Error(), "invalid action key (150 characters): lowercase words of letters, digits and _")
	s.Require().Contains(err.Error(), "at most 100 characters in all")
	_, err = c.Propose(ctx, &agentv1.ProposeRequest{Action: "test.manual", Title: "T", PayloadJson: `{}`, RunId: "run 7", IdempotencyKey: "k", Rationale: "Why."})
	s.Require().Equal(connect.CodeInvalidArgument, connect.CodeOf(err))
	s.Require().Contains(err.Error(), "invalid run id (5 characters): 1 to 100 letters, digits and . _ : -")
	_, err = c.ListProposals(ctx, &agentv1.ListProposalsRequest{RunId: "run 7"})
	s.Require().Equal(connect.CodeInvalidArgument, connect.CodeOf(err))
	s.Require().Contains(err.Error(), "invalid run id (5 characters): 1 to 100 letters, digits and . _ : -")
	_, err = c.GetProposal(ctx, &agentv1.GetProposalRequest{ProposalId: "p-1"})
	s.Require().Equal(connect.CodeInvalidArgument, connect.CodeOf(err))
	s.Require().Contains(err.Error(), "invalid proposal id (3 characters): a UUID")
	for _, payload := range []string{``, `[1]`, `{"a":`, `"text"`} {
		_, err = propose("k-"+payload, payload)
		s.Require().Equal(connect.CodeInvalidArgument, connect.CodeOf(err), payload)
	}
	first, err := propose("k-1", `{"n":1}`)
	s.Require().NoError(err)
	_, err = propose("k-1", `{"n":2}`)
	s.Require().Equal(connect.CodeAlreadyExists, connect.CodeOf(err), "a key used for another request")
	id := first.GetProposal().GetProposalId()
	withdrawn, err := c.WithdrawProposal(ctx, &agentv1.WithdrawProposalRequest{ProposalId: id, Reason: "Not needed."})
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusWithdrawn, withdrawn.GetProposal().GetStatus())
	_, err = c.WithdrawProposal(ctx, &agentv1.WithdrawProposalRequest{ProposalId: id})
	s.Require().Equal(connect.CodeFailedPrecondition, connect.CodeOf(err), "no longer waiting")
	_, err = c.GetProposal(ctx, &agentv1.GetProposalRequest{ProposalId: uuid.NewString()})
	s.Require().Equal(connect.CodeNotFound, connect.CodeOf(err))
	_, err = c.ListProposals(ctx, &agentv1.ListProposalsRequest{Status: "lost"})
	s.Require().Equal(connect.CodeInvalidArgument, connect.CodeOf(err))
	s.Require().Contains(err.Error(), "invalid status (4 characters): one of pending, approved, rejected, executed, withdrawn, expired, would_allow or dismissed")

	_, err = s.W.Kernel.Agents.SetLimits(t.Owner, t.AgentId, 1, 200)
	s.Require().NoError(err)
	// One proposal this minute already (k-1): the next is past the limit.
	_, err = propose("k-2", `{}`)
	s.Require().Equal(connect.CodeResourceExhausted, connect.CodeOf(err))
	seconds, ok := agentapi.RetryAfterOf(err)
	s.Require().True(ok, "the wait is in the details")
	s.Require().Positive(seconds)
	s.Require().Contains(err.Error(), "nothing was stored")
}

// A call too many in a burst is refused before it runs or is logged, over
// HTTP and MCP alike, since both share the token's rate.
func (s *AgentApiSuite) Test_TheRateIsSharedByBothDoors() {
	t := s.W.NewTenant(s.T(), "Acme")
	c := s.client(t.Token)
	// The MCP session is open before the rate is used up: its handshake
	// takes time, and the bucket refills one call every 200ms.
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: s.Mcp.URL, HTTPClient: &http.Client{Transport: bearer{token: t.Token}}, MaxRetries: -1,
	}, nil)
	s.Require().NoError(err)
	defer session.Close() //nolint:errcheck

	drain := func() error {
		for range 40 {
			if _, err := c.ListProposals(context.Background(), &agentv1.ListProposalsRequest{}); err != nil {
				return err
			}
		}
		return nil
	}
	limited := drain()
	s.Require().Equal(connect.CodeResourceExhausted, connect.CodeOf(limited))
	seconds, ok := agentapi.RetryAfterOf(limited)
	s.Require().True(ok)
	s.Require().Equal(1, seconds)
	s.Require().Less(len(s.toolCalls(t)), 40, "a refused call is not logged")

	// Right after the HTTP door is refused, the MCP door is too. On a slow
	// machine the bucket may refill a call in between, so it is tried a
	// few times; a rate that is not shared would never refuse it.
	var structured []byte
	for range 5 {
		result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "list_proposals"})
		s.Require().NoError(err)
		if result.IsError {
			structured, err = json.Marshal(result.StructuredContent)
			s.Require().NoError(err)
			break
		}
		_ = drain()
	}
	s.Require().NotNil(structured, "the MCP door shares the HTTP door's rate")
	s.Require().Contains(string(structured), `"code":"rate_limited"`)
	s.Require().Contains(string(structured), `"retryAfterSeconds":1`)
}

// toolCalls are the agent's calls in the tenant's log.
func (s *AgentApiSuite) toolCalls(t testfix.Tenant) []events.Stored {
	s.T().Helper()
	var calls []events.Stored
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		logged, err := events.List(t.Owner, tx, &t.AgentId, 0)
		for _, e := range logged {
			if e.Type == "agent.tool_called" {
				calls = append(calls, e)
			}
		}
		return err
	}))
	return calls
}

// Every answer says whether it is a dry run, false included, and a dry run
// tells the agent to act itself; a live one never does.
func (s *AgentApiSuite) Test_EveryAnswerSaysDryRun() {
	t := s.W.NewTenant(s.T(), "Acme")
	dry := s.W.DryRunAgent(s.T(), t)
	_, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &dry.Id, Actions: []string{"test.low"}, MaxRisk: mandates.RiskLow, BudgetCount: 5, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)

	live := s.mcpCall(t.Token, "propose", map[string]any{"action": "test.manual", "title": "T", "payload": map[string]any{}, "idempotency_key": "k-1", "rationale": "Why."})
	var answer map[string]any
	s.Require().NoError(json.Unmarshal([]byte(live), &answer))
	s.Require().Equal(false, answer["proposal"].(map[string]any)["dryRun"], "false is said, not left out")
	s.Require().NotContains(answer["proposal"].(map[string]any)["next"], "own credential")

	waiting := s.mcpCall(dry.Token, "propose", map[string]any{"action": "test.manual", "title": "T", "payload": map[string]any{}, "idempotency_key": "k-1", "rationale": "Why."})
	s.Require().NoError(json.Unmarshal([]byte(waiting), &answer))
	p := answer["proposal"].(map[string]any)
	s.Require().Equal(true, p["dryRun"])
	s.Require().Equal(proposals.StatusPending, p["status"])
	s.Require().Contains(p["outcome"], "dry run: a person would have decided it")
	s.Require().Contains(p["next"], "Do it yourself with your own credential, once")
	s.Require().NotEmpty(p["createdAt"], "a dry run has its time like any proposal")

	// A repeat, and every later read, never tells the agent to act again.
	repeat := s.mcpCall(dry.Token, "propose", map[string]any{"action": "test.manual", "title": "T", "payload": map[string]any{}, "idempotency_key": "k-1", "rationale": "Why."})
	s.Require().NoError(json.Unmarshal([]byte(repeat), &answer))
	s.Require().Equal(true, answer["repeat"])
	s.Require().NotContains(answer["proposal"].(map[string]any)["next"], "Do it yourself")
	s.Require().Contains(answer["proposal"].(map[string]any)["next"], "do not do it again")
	reread := s.mcpCall(dry.Token, "get_proposal", map[string]any{"proposal_id": p["proposalId"]})
	s.Require().NoError(json.Unmarshal([]byte(reread), &answer))
	s.Require().NotContains(answer["proposal"].(map[string]any)["next"], "Do it yourself")

	// Without a job queue here, the kernel's execution is run by hand: the
	// mandate would allow it.
	allowed, err := s.client(dry.Token).Propose(context.Background(), &agentv1.ProposeRequest{Action: "test.low", Title: "T", PayloadJson: `{}`, IdempotencyKey: "k-2", Rationale: "Why."})
	s.Require().NoError(err)
	s.Require().True(allowed.GetProposal().GetDryRun())
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, uuid.MustParse(allowed.GetProposal().GetProposalId())))
	read, err := s.client(dry.Token).GetProposal(context.Background(), &agentv1.GetProposalRequest{ProposalId: allowed.GetProposal().GetProposalId(), WaitSeconds: 30})
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusWouldAllow, read.GetProposal().GetStatus())
	s.Require().NotContains(read.GetProposal().GetNext(), "Do it yourself")
	s.Require().Contains(read.GetProposal().GetOutcome(), "a mandate would have allowed it")
	s.Require().NotNil(read.GetProposal().GetExecutionBasis())
	s.Require().Nil(read.GetProposal().GetDelivery())

	start := time.Now()
	pending, err := s.client(dry.Token).GetProposal(context.Background(), &agentv1.GetProposalRequest{ProposalId: p["proposalId"].(string), WaitSeconds: 30})
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusPending, pending.GetProposal().GetStatus())
	s.Require().Less(time.Since(start), 5*time.Second, "nobody decides a dry run, so a read does not wait for one")

	listed, err := s.client(dry.Token).ListProposals(context.Background(), &agentv1.ListProposalsRequest{Status: "would_allow"})
	s.Require().NoError(err)
	s.Require().Len(listed.GetProposals(), 1)
}
