// Package agentapi is the agent contract, mandate.agent.v1: the one
// implementation of what an agent may do. An agent lists the actions it
// may propose and what its mandates allow, proposes, reads the outcome, and
// withdraws what still waits. It never approves, rejects or executes
// anything.
//
// The same service is served two ways: as JSON over HTTP by Connect
// (Handler), and over MCP by package mcp, which calls it in process. Both
// share the rate limit per token, the waits and the guidance texts, and
// every call made with a valid token, unless refused for its rate, is
// recorded in the tenant's log with the door it came through.
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/time/rate"

	agentv1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/agent/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/agent/v1/agentv1connect"
	"github.com/andreas-lindfalk/pgmandate/modules/agents"
	"github.com/andreas-lindfalk/pgmandate/modules/deliveries"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// Host is what the kernel gives the agent contract.
type Host interface {
	AgentsService() *agents.Service
	ProposalsService() *proposals.Service
	// ActionsOf lists the actions of the tenant in ctx.
	ActionsOf(ctx context.Context) ([]proposals.Action, error)
	Lookup(ctx context.Context, key string) (proposals.Action, bool, error)
	LiveMandates(ctx context.Context) ([]mandates.Mandate, error)
	// SpentCount is how many executions each mandate has allowed this month.
	SpentCount(ctx context.Context, mandateIds []uuid.UUID) (map[uuid.UUID]int64, error)
	// DeliveryOf is the delivery of a decided proposal, if it has one.
	DeliveryOf(ctx context.Context, proposalId uuid.UUID) (deliveries.Delivery, bool, error)
	DeliveriesOf(ctx context.Context, proposalIds []uuid.UUID) (map[uuid.UUID]deliveries.Delivery, error)
	// Executes says whether proposals can be executed at all (a queue runs).
	Executes() bool
	// Unmet says, for a proposal no mandate's condition allowed, what in the
	// payload stood in the way, per mandate.
	Unmet(ctx context.Context, agentId uuid.UUID, action, risk string, payload json.RawMessage) ([]string, error)
}

const (
	// outcomeWait is how long Propose waits for an execution before it
	// answers with the proposal still pending. Short, since some MCP
	// clients give up on a call after a few seconds; a decision a mandate
	// covers takes far less.
	outcomeWait = 2 * time.Second
	outcomePoll = 100 * time.Millisecond

	// MaxWait bounds how long GetProposal waits when asked to.
	MaxWait = 50 * time.Second
)

// The doors a call comes through, as the log records them.
const (
	ViaMcp  = "mcp"
	ViaHttp = "http"
)

// Call is who calls, and how: the agent's grant from its token, the door,
// and for MCP the client as it introduced itself.
type Call struct {
	Grant agents.Grant
	Via   string
	// Client and ClientVersion, when set, are the MCP client's own, and
	// replace what the request's provenance says.
	Client        string
	ClientVersion string
}

type callKey struct{}

// WithCall marks ctx as a call by an authenticated agent.
func WithCall(ctx context.Context, call Call) context.Context {
	return context.WithValue(ctx, callKey{}, call)
}

func callFrom(ctx context.Context) (Call, bool) {
	call, ok := ctx.Value(callKey{}).(Call)
	return call, ok
}

type Service struct {
	host Host

	mu       sync.Mutex
	limiters map[uuid.UUID]*limiter
}

var _ agentv1connect.AgentServiceHandler = (*Service)(nil)

func New(host Host) *Service {
	return &Service{host: host, limiters: map[uuid.UUID]*limiter{}}
}

// Authenticate reads an agent's bearer token. Anything else, a person's
// session among them, is unauthenticated.
func (s *Service) Authenticate(ctx context.Context, token string) (agents.Grant, error) {
	return s.host.AgentsService().Authenticate(ctx, token)
}

type limiter struct {
	*rate.Limiter
	lastSeen time.Time
}

// Limiters of tokens not seen for limiterIdle are dropped once there are
// more than limiterSweepAt of them, so the map does not grow with every
// token ever used.
const (
	limiterIdle    = 10 * time.Minute
	limiterSweepAt = 1024
)

// callsPerSecond and callBurst bound each token's calls, over MCP and HTTP
// together.
const (
	callsPerSecond = 5
	callBurst      = 30
)

func (s *Service) limiter(tokenId uuid.UUID) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.limiters) > limiterSweepAt {
		for id, l := range s.limiters {
			if now.Sub(l.lastSeen) > limiterIdle {
				delete(s.limiters, id)
			}
		}
	}
	l, ok := s.limiters[tokenId]
	if !ok {
		l = &limiter{Limiter: rate.NewLimiter(callsPerSecond, callBurst)}
		s.limiters[tokenId] = l
	}
	l.lastSeen = now
	return l.Limiter
}

// Arguments are checked against these before a call runs, and only values
// that match are kept in the chain: an agent cannot write free text, such
// as personal data, into the permanent record.
var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	actionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	runPattern    = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)
	statusPattern = regexp.MustCompile(`^(?:pending|approved|rejected|executed|withdrawn|expired|would_allow|dismissed)$`)
)

// maxArgument bounds any id-like argument before it is matched or kept.
const maxArgument = 200

func requireMatch(value string, pattern *regexp.Regexp, what string) error {
	if len(value) > maxArgument || !pattern.MatchString(value) {
		return fault.New(fault.InvalidArgument, fmt.Sprintf("invalid %s (%d characters)", what, utf8.RuneCountInString(value)))
	}
	return nil
}

func logged(value string, pattern *regexp.Regexp) any {
	if value == "" || (len(value) <= maxArgument && pattern.MatchString(value)) {
		return value
	}
	return map[string]any{"invalidChars": utf8.RuneCountInString(value)}
}

// serve runs one call: it is slowed down past the rate, scoped to the
// agent and its run, run, and recorded in the log before any answer
// leaves. If the call cannot be recorded, nothing is returned.
func serve[Resp any](s *Service, ctx context.Context, name string, arguments map[string]any, runId string, do func(context.Context) (*Resp, error)) (*Resp, error) {
	call, ok := callFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("an agent token is required, as Authorization: Bearer mnd_<token>"))
	}
	// Every call goes into the permanent chain, so a runaway agent is
	// slowed down before it is recorded.
	if !s.limiter(call.Grant.TokenId).Allow() {
		slog.Warn("agent call rate limited", "call", name, "token", call.Grant.TokenId, "via", call.Via)
		return nil, rateLimited("too many calls; wait a second and try again", 1)
	}
	scoped := call.Grant.Context(ctx)
	// A valid run id tags the call's own event too, so the chain can group
	// a run's calls with the proposals they made.
	if runId != "" && len(runId) <= maxArgument && runPattern.MatchString(runId) {
		scoped = actor.WithRun(scoped, runId)
	}
	out, err := do(scoped)
	arguments["via"] = call.Via
	// Recorded even when the caller has gone away meanwhile, as from a long
	// wait: what it did, such as a proposal stored, still happened.
	if logErr := s.host.AgentsService().Called(context.WithoutCancel(scoped), call.Grant, name, arguments, err != nil); logErr != nil {
		slog.Error("record agent call", "call", name, "error", logErr)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	if err != nil {
		return nil, toConnect(name, err)
	}
	return out, nil
}

// toConnect gives the agent the reason for an expected failure and hides an
// internal one. A rate limit carries how long to wait.
func toConnect(name string, err error) error {
	if errors.Is(err, context.Canceled) {
		// The caller went away. As after an internal error, a proposal may
		// have been stored; the same call under the same key returns it.
		return connect.NewError(connect.CodeCanceled, errors.New("the call was canceled; anything it stored is returned by the same call under the same key"))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("the call ran out of time; anything it stored is returned by the same call under the same key"))
	}
	if fault.CodeOf(err) == fault.RateLimited {
		// Nothing was stored, and the same call later, under the same key,
		// is the way on. The database says when a place opens.
		retry := 60
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if seconds, parseErr := strconv.Atoi(pgErr.Hint); parseErr == nil && seconds > 0 {
				retry = seconds
			}
		}
		return rateLimited(fault.Message(err)+"; nothing was stored: send the same call again later, under the same idempotency_key", retry)
	}
	if fault.CodeOf(err) == fault.Internal {
		slog.Error("agent call failed", "call", name, "error", err)
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return fault.ToConnect(err)
}

func rateLimited(message string, seconds int) error {
	e := connect.NewError(connect.CodeResourceExhausted, errors.New(message))
	if detail, err := connect.NewErrorDetail(&agentv1.RetryAfter{Seconds: int32(seconds)}); err == nil { //nolint:gosec // seconds to wait, small
		e.AddDetail(detail)
	}
	return e
}

// RetryAfterOf reads the seconds to wait from a resource_exhausted error.
func RetryAfterOf(err error) (int, bool) {
	var e *connect.Error
	if !errors.As(err, &e) || e.Code() != connect.CodeResourceExhausted {
		return 0, false
	}
	for _, detail := range e.Details() {
		value, err := detail.Value()
		if err != nil {
			continue
		}
		if retry, ok := value.(*agentv1.RetryAfter); ok {
			return int(retry.GetSeconds()), true
		}
	}
	return 0, false
}
