// Package proposals is the queue: the one way an agent changes anything.
// An agent proposes an action with a payload; a person approves, edits or
// rejects it; or the system executes it under a mandate. The database
// enforces every rule here a second time, so no code path around this
// package can do what this package refuses.
package proposals

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
	"github.com/andreas-lindfalk/pgmandate/pkg/injection"
)

const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	// StatusExecuted marks a proposal the system carried out under a
	// mandate, without a person.
	StatusExecuted = "executed"

	// FlagSteeringText marks a proposal whose title or payload holds text
	// that tries to steer a model. Such a proposal is high risk and is never
	// executed without a person.
	FlagSteeringText = "steering_text"

	maxTitle     = 200
	maxNote      = 1000
	maxListLimit = 500

	// rapidApprovalLimit approvals within a minute by one person is written
	// to the chain: an auditor's red flag is rubber-stamping.
	rapidApprovalLimit = 10
)

// Action is what a proposal can do, as the owner defined it or the host
// registered it. The host owns the meaning; mandate owns the gate.
type Action struct {
	Key         string
	Description string
	// Risk is the default risk of the action: low, medium or high.
	Risk string
	// CostUnits is charged to the mandate's budget when the action is
	// executed without a person.
	CostUnits int64
	// MayExecute says whether the action may ever be carried out under a
	// mandate. False means a person always decides, whatever the mandates.
	MayExecute bool
	// Schema is the JSON schema of the payload, shown to agents.
	Schema json.RawMessage
	// Validate checks a payload and returns the payload to store. Set by
	// the kernel from Schema.
	Validate func(payload json.RawMessage) (json.RawMessage, error)
	// Assess may raise or lower the risk from the payload, such as a refund
	// above an amount. It can never lower a flagged proposal below high.
	Assess func(payload json.RawMessage) string
	// Apply carries a builtin action out, inside the transaction that
	// approves or executes the proposal. Write host rows that name the
	// proposal and guard them with mandate.may_write_for; queue a job for
	// anything that leaves the database.
	Apply func(ctx context.Context, tx pgx.Tx, proposal Proposal) error
	// Webhook is the connector of an action defined as data: the decision
	// is delivered to it by a job after it is committed.
	Webhook *Webhook
	// Department is an optional label from the action's definition.
	Department string
}

// Webhook is where a decided proposal is delivered.
type Webhook struct {
	Url string
}

// Applicable says whether a decision on the action can be carried out
// here: by builtin code or by a delivery.
func (e Action) Applicable() bool {
	return e.Apply != nil || e.Webhook != nil
}

// Actions finds actions for the tenant in scope. LookupIn runs inside the
// caller's transaction: a lookup that opened a second connection while a
// transaction holds one would exhaust the pool under load.
type Actions interface {
	Lookup(ctx context.Context, key string) (Action, bool, error)
	LookupIn(ctx context.Context, tx pgx.Tx, key string) (Action, bool, error)
}

// Deliver records a delivery of a decided proposal to its webhook, in the
// deciding transaction. The kernel provides it.
type Deliver func(ctx context.Context, tx pgx.Tx, proposal Proposal, action Action) error

type Proposal struct {
	Id      uuid.UUID
	AgentId uuid.UUID
	RunId   string
	Action  string
	Risk    string
	Title   string
	Payload json.RawMessage
	// ProposedPayload is the payload as the agent proposed it, before any
	// edit.
	ProposedPayload json.RawMessage
	CostUnits       int64
	Flags           []string
	IdempotencyKey  string
	Status          string
	Edited          bool
	DecidedBy       *uuid.UUID
	DecidedAt       *string
	DecisionNote    string
	CreatedAt       string
	// ExecutionBasis is set when the status is executed.
	ExecutionBasis *ExecutionBasis
}

// ExecutionBasis is what allowed a proposal to be executed without a
// person: the mandate, the policy it rests on, its risk limit and its
// budget at the time. The database checks all of it against the mandate
// when the status changes: the id, the policy reference, the risk limit,
// the budget, the cost and this month's spend.
type ExecutionBasis struct {
	MandateId   uuid.UUID `json:"mandateId"`
	PolicyRef   string    `json:"policyRef"`
	MaxRisk     string    `json:"maxRisk"`
	BudgetUnits int64     `json:"budgetUnits"`
	SpentUnits  int64     `json:"spentUnits"`
	CostUnits   int64     `json:"costUnits"`
}

type Input struct {
	Action  string
	Title   string
	Payload json.RawMessage
	// IdempotencyKey names this one request and is required. The same key
	// from the same agent is the same proposal: a repeat returns the first
	// one, whatever has happened to it since, and the same key with another
	// action or payload is refused.
	IdempotencyKey string
}

type Decision struct {
	Title   string
	Payload json.RawMessage
	// Confirm must be true to approve a high risk proposal.
	Confirm bool
	Note    string
}

type Filter struct {
	Status  string
	AgentId *uuid.UUID
	// RunId keeps the proposals the agent made under one run id.
	RunId string
	// OldestFirst orders a queue as it is worked.
	OldestFirst bool
	Limit       int
}

// AfterStore runs in the transaction that stores a proposal that may be
// executed under a mandate. The kernel uses it to queue the execution, so
// the job exists if and only if the proposal does.
type AfterStore func(ctx context.Context, tx pgx.Tx, proposalId uuid.UUID) error

// BasisFor decides, inside the execution transaction, whether a mandate
// allows the proposal now: the basis to record, or the reason it does not.
type BasisFor func(ctx context.Context, tx pgx.Tx, proposal Proposal) (ExecutionBasis, string, error)

type Service struct {
	pool       *pgxpool.Pool
	actions    Actions
	afterStore AfterStore
	deliver    Deliver
}

func NewService(pool *pgxpool.Pool, actions Actions) *Service {
	return &Service{pool: pool, actions: actions}
}

// SetAfterStore connects the execution queue. Nil means nothing is ever
// executed and every proposal waits for a person.
func (s *Service) SetAfterStore(hook AfterStore) {
	s.afterStore = hook
}

// SetDeliver connects the deliveries. Nil means webhook actions cannot be
// decided here.
func (s *Service) SetDeliver(deliver Deliver) {
	s.deliver = deliver
}

// apply carries a decision out: builtin code inside the transaction, or a
// delivery recorded in it.
func (s *Service) apply(ctx context.Context, tx pgx.Tx, action Action, proposal Proposal) error {
	switch {
	case action.Apply != nil:
		return action.Apply(ctx, tx, proposal)
	case action.Webhook != nil && s.deliver != nil:
		return s.deliver(ctx, tx, proposal, action)
	}
	return fault.New(fault.InvalidArgument, "the action "+action.Key+" has no connector here")
}

// Propose stores a proposal as the agent in scope. The payload is held to
// the action's schema, and text that tries to steer a model flags the
// proposal and makes it high risk. Returns the proposal and whether it was
// new; a repeated idempotency key returns the first proposal.
func (s *Service) Propose(ctx context.Context, input Input) (Proposal, bool, error) {
	a, ok := actor.From(ctx)
	if !ok || a.Kind != actor.Agent {
		return Proposal{}, false, fault.New(fault.PermissionDenied, "only an agent proposes")
	}
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return Proposal{}, false, fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	if !mandates.ValidKey(input.Action) {
		return Proposal{}, false, fault.New(fault.InvalidArgument, "invalid action key")
	}
	switch {
	case strings.TrimSpace(input.IdempotencyKey) == "":
		return Proposal{}, false, fault.New(fault.InvalidArgument, "an idempotency key is required: it names this one request, so that sending it again returns the first proposal")
	case !keyPattern.MatchString(input.IdempotencyKey):
		return Proposal{}, false, fault.New(fault.InvalidArgument, "the idempotency key is 1 to 200 printable ASCII characters without spaces, such as run-9:step-3")
	}
	if err := checkNumbers(input.Payload); err != nil {
		return Proposal{}, false, err
	}
	request, err := requestHash(input.Action, input.Payload)
	if err != nil {
		return Proposal{}, false, fault.New(fault.InvalidArgument, "the payload must be a JSON object")
	}
	// A repeat is answered before anything is checked against the action
	// as it is today: the action may have been removed or its schema
	// changed since, and the agent still has to learn that its request was
	// already taken, and what became of it.
	if first, found, err := s.repeated(ctx, a.Id, input.IdempotencyKey, request); err != nil || found {
		return first, false, err
	}
	action, ok, err := s.actions.Lookup(ctx, input.Action)
	if err != nil {
		return Proposal{}, false, err
	}
	if !ok {
		return Proposal{}, false, fault.New(fault.InvalidArgument, "unknown action: "+input.Action+"; call list_actions for the actions you may propose")
	}
	if !action.Applicable() {
		return Proposal{}, false, fault.New(fault.InvalidArgument, "the action "+action.Key+" has no connector here")
	}
	title := strings.TrimSpace(input.Title)
	if title == "" || utf8.RuneCountInString(title) > maxTitle {
		return Proposal{}, false, fault.New(fault.InvalidArgument, fmt.Sprintf("the title must be 1 to %d characters", maxTitle))
	}
	payload, object, err := checkPayload(action, input.Payload)
	if err != nil {
		return Proposal{}, false, err
	}
	flags := []string{}
	risk := action.Risk
	if action.Assess != nil {
		risk = action.Assess(payload)
	}
	if mandates.RiskRank(risk) < 0 {
		return Proposal{}, false, fmt.Errorf("action %s assessed an unknown risk %q", action.Key, risk)
	}
	// An agent may have been steered by what it read. The same text that
	// would steer a model is flagged here, and the flag makes the proposal
	// high risk: a person looks closer, and no mandate covers it.
	if signals := injection.Signals(append(stringsOf(object), title)...); len(signals) > 0 {
		flags = append(flags, FlagSteeringText)
		risk = mandates.RiskHigh
	}

	var result Proposal
	created := false
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		id, err := q.InsertProposal(ctx, repo.InsertProposalParams{
			Id: uuid.New(), TenantId: tenantId, AgentId: a.Id, Action: action.Key, Risk: risk, Title: title,
			Payload: payload, CostUnits: action.CostUnits, Flags: flags, IdempotencyKey: input.IdempotencyKey, RequestHash: request,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Another copy of the request got there first, between the
			// check above and this insert.
			row, err := q.GetByIdempotencyKey(ctx, repo.GetByIdempotencyKeyParams{AgentId: a.Id, IdempotencyKey: input.IdempotencyKey})
			if err != nil {
				return err
			}
			result, err = sameRequest(row, request)
			return err
		}
		if err != nil {
			return err
		}
		created = true
		row, err := q.GetProposal(ctx, id)
		if err != nil {
			return err
		}
		result = toProposal(row)
		// The chain is permanent: the agent's title and payload stay on the
		// proposal row, never in the chain. The chain holds the payload's
		// digest, so a payload changed in the table later shows.
		payloadDigest, err := events.Digest(result.Payload)
		if err != nil {
			return err
		}
		if _, err := events.Append(ctx, tx, events.Event{
			Type: "proposal.created", SubjectId: id,
			Payload: map[string]any{
				"action": action.Key, "risk": risk, "flags": flags, "costUnits": action.CostUnits, "titleChars": utf8.RuneCountInString(title),
				"payloadSha256": payloadDigest, "requestSha256": hex.EncodeToString(request),
			},
		}); err != nil {
			return err
		}
		if s.afterStore != nil && Candidate(action, result) {
			return s.afterStore(ctx, tx, id)
		}
		return nil
	})
	if err != nil {
		return Proposal{}, false, fault.FromDb(err)
	}
	return result, created, nil
}

// Candidate says whether a proposal, as stored, can be executed under a
// mandate at all: the action allows it, the risk is below high and nothing
// was flagged. The mandates decide the rest at execution time.
func Candidate(action Action, proposal Proposal) bool {
	return WhyNotCandidate(action, proposal) == ""
}

// WhyNotCandidate is the reason a proposal can never be executed, as the
// skip reasons name it, or empty when it is a candidate.
func WhyNotCandidate(action Action, proposal Proposal) string {
	switch {
	case !action.MayExecute || !action.Applicable():
		return SkipNotExecutable
	case len(proposal.Flags) > 0:
		return SkipFlagged
	case proposal.Risk == mandates.RiskHigh:
		return mandates.SkipRiskAbove
	}
	return ""
}

func checkPayload(action Action, payload json.RawMessage) (json.RawMessage, map[string]any, error) {
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return nil, nil, fault.New(fault.InvalidArgument, "the payload must be a JSON object")
	}
	if action.Validate != nil {
		cleaned, err := action.Validate(payload)
		if err != nil {
			return nil, nil, fault.New(fault.InvalidArgument, "the payload does not match the action's schema: "+err.Error())
		}
		payload = cleaned
		if err := json.Unmarshal(payload, &object); err != nil || object == nil {
			return nil, nil, fault.New(fault.InvalidArgument, "the payload must be a JSON object")
		}
	}
	return payload, object, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Proposal, error) {
	var proposal Proposal
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := repo.New(tx).GetProposal(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "proposal not found")
		}
		proposal = toProposal(row)
		return err
	})
	return proposal, err
}

func (s *Service) List(ctx context.Context, filter Filter) ([]Proposal, error) {
	limit := filter.Limit
	if limit <= 0 || limit > maxListLimit {
		limit = maxListLimit
	}
	params := repo.ListProposalsParams{MaxRows: int32(limit), AgentId: filter.AgentId, OldestFirst: filter.OldestFirst} //nolint:gosec // bounded above
	if filter.Status != "" {
		params.Status = &filter.Status
	}
	if filter.RunId != "" {
		params.RunId = &filter.RunId
	}
	var result []Proposal
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).ListProposals(ctx, params)
		for _, row := range rows {
			result = append(result, toProposal(row))
		}
		return err
	})
	return result, err
}

// Count is how many proposals of the tenant in scope have the status, or
// how many there are at all when the status is empty.
func (s *Service) Count(ctx context.Context, status string) (int64, error) {
	var count int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var filter *string
		if status != "" {
			filter = &status
		}
		var err error
		count, err = repo.New(tx).CountProposals(ctx, filter)
		return err
	})
	return count, err
}

// Decided lists every decided proposal, oldest decision first: the record
// of what people and mandates said, and the seed of an eval set.
func (s *Service) Decided(ctx context.Context) ([]Proposal, error) {
	var result []Proposal
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).ListDecided(ctx)
		for _, row := range rows {
			result = append(result, toProposal(row))
		}
		return err
	})
	return result, err
}

// History returns the events about a proposal, oldest first.
func (s *Service) History(ctx context.Context, id uuid.UUID) ([]events.Stored, error) {
	var result []events.Stored
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		result, err = events.List(ctx, tx, &id, 500)
		return err
	})
	return result, err
}

// SkipReasons is SkipReason for many proposals in one query.
func (s *Service) SkipReasons(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	result := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return result, nil
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).LastSkips(ctx, ids)
		for _, row := range rows {
			result[row.SubjectId] = row.Reason
		}
		return err
	})
	return result, err
}

// SkipReason is why the last attempt to execute the proposal left it for a
// person, if there was one.
func (s *Service) SkipReason(ctx context.Context, id uuid.UUID) (string, bool, error) {
	var reason string
	found := false
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		event, ok, err := events.Last(ctx, tx, id, "proposal.execution_skipped")
		if err != nil || !ok {
			return err
		}
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		reason, found = payload.Reason, true
		return nil
	})
	return reason, found, err
}

// Approve decides a proposal as approved, optionally with an edited title
// and payload, and lets the action apply it. Only a person may.
func (s *Service) Approve(ctx context.Context, id uuid.UUID, decision Decision) (Proposal, error) {
	return s.decide(ctx, id, StatusApproved, decision)
}

// Reject decides a proposal as rejected. Only a person may.
func (s *Service) Reject(ctx context.Context, id uuid.UUID, note string) (Proposal, error) {
	return s.decide(ctx, id, StatusRejected, Decision{Note: note})
}

func (s *Service) decide(ctx context.Context, id uuid.UUID, status string, decision Decision) (Proposal, error) {
	a, ok := actor.From(ctx)
	if !ok || a.Kind != actor.Person {
		return Proposal{}, fault.New(fault.PermissionDenied, "only a person decides a proposal")
	}
	if _, ok := actor.TenantFrom(ctx); !ok {
		return Proposal{}, fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	if utf8.RuneCountInString(strings.TrimSpace(decision.Title)) > maxTitle {
		return Proposal{}, fault.New(fault.InvalidArgument, fmt.Sprintf("the title can be at most %d characters", maxTitle))
	}
	if utf8.RuneCountInString(strings.TrimSpace(decision.Note)) > maxNote {
		return Proposal{}, fault.New(fault.InvalidArgument, fmt.Sprintf("the note can be at most %d characters", maxNote))
	}
	var result Proposal
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		row, err := q.GetProposalForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "proposal not found")
		}
		if err != nil {
			return err
		}
		if row.Status != StatusPending {
			return fault.New(fault.Conflict, "proposal is already decided")
		}
		action, ok, err := s.actions.LookupIn(ctx, tx, row.Action)
		if err != nil {
			return err
		}
		// A proposal for an action that has since been removed can still
		// be rejected; only an approval needs the action.
		if !ok && status == StatusApproved {
			return fault.New(fault.InvalidArgument, "the action "+row.Action+" is no longer defined; the proposal can only be rejected")
		}
		title, payload, risk := row.Title, json.RawMessage(row.Payload), row.Risk
		if status == StatusApproved {
			// A rejection takes a note only. An approval may carry an edit,
			// held to the same schema as the agent's proposal, and assessed
			// again: an edit can raise the risk, never lower it.
			if strings.TrimSpace(decision.Title) != "" {
				title = strings.TrimSpace(decision.Title)
			}
			if decision.Payload != nil {
				if err := checkNumbers(decision.Payload); err != nil {
					return err
				}
				if payload, _, err = checkPayload(action, decision.Payload); err != nil {
					return err
				}
			}
			// The payload is assessed again under the action as it is now:
			// an edit, or an action made riskier since the agent proposed,
			// can raise the risk the person approves at, never lower it.
			if current := currentRisk(action, payload); mandates.RiskRank(current) > mandates.RiskRank(risk) {
				risk = current
			}
			if decision.Payload == nil {
				if _, _, err := checkPayload(action, payload); err != nil {
					// The action's schema may have changed since the agent
					// proposed. What is approved fits the schema as it is now,
					// so nothing reaches a connector in a shape it no longer
					// accepts.
					return fault.Wrap(fault.Conflict, "the action's schema has changed since this was proposed, and "+fault.Message(err)+"; edit the payload to fit, or reject the proposal", err)
				}
			}
			if risk == mandates.RiskHigh && !decision.Confirm {
				return fault.New(fault.InvalidArgument, "a high risk proposal must be confirmed explicitly")
			}
		}
		var note *string
		if n := strings.TrimSpace(decision.Note); n != "" {
			note = &n
		}
		if err := q.DecideProposal(ctx, repo.DecideProposalParams{Id: id, Status: status, Title: title, Payload: payload, Risk: risk, DecisionNote: note}); err != nil {
			return err
		}
		decided, err := q.GetProposal(ctx, id)
		if err != nil {
			return err
		}
		result = toProposal(decided)
		// The decision is logged before its consequences, so the chain
		// reads in causal order. It names the payload decided on.
		payloadDigest, err := events.Digest(result.Payload)
		if err != nil {
			return err
		}
		decidedEvent := map[string]any{"action": result.Action, "edited": result.Edited, "note": result.DecisionNote, "risk": risk, "payloadSha256": payloadDigest}
		// An approval names the connector it was made against, so a later
		// move of the action shows as a move, not as what was approved.
		if status == StatusApproved && action.Webhook != nil {
			sum := sha256.Sum256([]byte(action.Webhook.Url))
			decidedEvent["connectorUrlSha256"] = hex.EncodeToString(sum[:])
		}
		if _, err := events.Append(ctx, tx, events.Event{
			Type: "proposal." + status, SubjectId: id, Payload: decidedEvent,
		}); err != nil {
			return err
		}
		if status != StatusApproved {
			return nil
		}
		if err := s.apply(ctx, tx, action, result); err != nil {
			return fmt.Errorf("apply %s: %w", result.Action, err)
		}
		return flagRapidApprovals(ctx, tx, q, a.Id)
	})
	return result, fault.FromDb(err)
}

// Exported is one line of a decisions export: what was proposed and what
// was decided, by a person or by a mandate. Hosts turn these into
// regression fixtures for the agents that propose.
type Exported struct {
	ProposalId     uuid.UUID       `json:"proposalId"`
	AgentId        uuid.UUID       `json:"agentId"`
	RunId          string          `json:"runId,omitempty"`
	Action         string          `json:"action"`
	Risk           string          `json:"risk"`
	Flags          []string        `json:"flags"`
	Title          string          `json:"title"`
	Payload        json.RawMessage `json:"payload"`
	Outcome        string          `json:"outcome"`
	Edited         bool            `json:"edited"`
	DecisionNote   string          `json:"decisionNote,omitempty"`
	ExecutionBasis *ExecutionBasis `json:"executionBasis,omitempty"`
	DecidedAt      string          `json:"decidedAt"`
}

// WriteDecisions writes decided proposals as JSON lines.
func WriteDecisions(w io.Writer, list []Proposal) error {
	encoder := json.NewEncoder(w)
	for _, p := range list {
		decidedAt := ""
		if p.DecidedAt != nil {
			decidedAt = *p.DecidedAt
		}
		if err := encoder.Encode(Exported{
			ProposalId: p.Id, AgentId: p.AgentId, RunId: p.RunId, Action: p.Action, Risk: p.Risk, Flags: p.Flags, Title: p.Title,
			Payload: p.Payload, Outcome: p.Status, Edited: p.Edited, DecisionNote: p.DecisionNote, ExecutionBasis: p.ExecutionBasis, DecidedAt: decidedAt,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Reasons an execution is skipped, besides what the mandates say.
const (
	SkipNotExecutable = "action_not_executable"
	// SkipPayloadInvalid is written when the payload no longer fits the
	// action's schema, which changed after the agent proposed.
	SkipPayloadInvalid = "payload_invalid"
	// SkipActionChanged is written when the action's cost or risk changed
	// after the agent proposed.
	SkipActionChanged = "action_changed"
	SkipFlagged       = "flagged"
)

// ErrActionFailed wraps an error from the action's Apply under an
// execution, so the caller can tell it from a refusal of the execution
// itself. The proposal is still pending.
var ErrActionFailed = errors.New("the action failed")

// Execute carries a pending proposal out as the system, under the mandate
// basisFor finds inside the same transaction, which also locks the mandate
// so that budgets are counted one execution at a time. The action is
// applied in that transaction; if it fails, nothing is recorded and the
// proposal stays pending. When no mandate allows it now, the reason is
// written to the chain (proposal.execution_skipped) and returned, and the
// proposal waits for a person. The database checks the same rules again
// when the status changes.
func (s *Service) Execute(ctx context.Context, id uuid.UUID, basisFor BasisFor) (Proposal, string, error) {
	a, ok := actor.From(ctx)
	if !ok || a.Kind != actor.System {
		return Proposal{}, "", fault.New(fault.PermissionDenied, "only the system executes a proposal")
	}
	if _, ok := actor.TenantFrom(ctx); !ok {
		return Proposal{}, "", fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	var result Proposal
	skipped := ""
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('mandate.executing_proposal', $1, true)`, id.String()); err != nil {
			return err
		}
		q := repo.New(tx)
		row, err := q.GetProposalForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "proposal not found")
		}
		if err != nil {
			return err
		}
		if row.Status != StatusPending {
			return fault.New(fault.Conflict, "proposal is already decided")
		}
		proposal := toProposal(row)
		skip := func(reason string) error {
			skipped = reason
			_, err := events.Append(ctx, tx, events.Event{
				Type: "proposal.execution_skipped", SubjectId: id, Payload: map[string]any{"reason": reason, "action": proposal.Action},
			})
			return err
		}
		action, ok, err := s.actions.LookupIn(ctx, tx, proposal.Action)
		if err != nil {
			return err
		}
		switch {
		case !ok || !action.MayExecute || !action.Applicable():
			return skip(SkipNotExecutable)
		case len(proposal.Flags) > 0:
			return skip(SkipFlagged)
		case proposal.Risk == mandates.RiskHigh:
			return skip(mandates.SkipRiskAbove)
		}
		if _, _, err := checkPayload(action, proposal.Payload); err != nil {
			return skip(SkipPayloadInvalid)
		}
		// The action as it is now: a cost changed since, or a risk raised
		// above the proposal's, is for a person to look at.
		if action.CostUnits != proposal.CostUnits || mandates.RiskRank(currentRisk(action, proposal.Payload)) > mandates.RiskRank(proposal.Risk) {
			return skip(SkipActionChanged)
		}
		basis, reason, err := basisFor(ctx, tx, proposal)
		if err != nil {
			return err
		}
		if reason != "" {
			return skip(reason)
		}
		basisJson, err := json.Marshal(basis)
		if err != nil {
			return err
		}
		if err := q.ExecuteProposal(ctx, repo.ExecuteProposalParams{Id: id, Basis: basisJson}); err != nil {
			return err
		}
		executed, err := q.GetProposal(ctx, id)
		if err != nil {
			return err
		}
		result = toProposal(executed)
		payloadDigest, err := events.Digest(result.Payload)
		if err != nil {
			return err
		}
		if _, err := events.Append(ctx, tx, events.Event{
			Type: "proposal.executed", SubjectId: id,
			Payload: map[string]any{"action": result.Action, "risk": result.Risk, "costUnits": result.CostUnits, "basis": basis, "payloadSha256": payloadDigest},
		}); err != nil {
			return err
		}
		if err := s.apply(ctx, tx, action, result); err != nil {
			return fmt.Errorf("%w: apply %s: %w", ErrActionFailed, result.Action, err)
		}
		return nil
	})
	if err != nil {
		return Proposal{}, "", fault.FromDb(err)
	}
	return result, skipped, nil
}

// RecordSkip writes a skip reason to the chain outside an execution, for
// when the database refused the execution itself.
func (s *Service) RecordSkip(ctx context.Context, id uuid.UUID, reason string) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := events.Append(ctx, tx, events.Event{
			Type: "proposal.execution_skipped", SubjectId: id, Payload: map[string]any{"reason": reason},
		})
		return err
	})
}

func flagRapidApprovals(ctx context.Context, tx pgx.Tx, q *repo.Queries, personId uuid.UUID) error {
	// Written once, when the limit is reached: a red flag that repeats on
	// every further approval would only be noise.
	count, err := q.CountRecentApprovals(ctx, &personId)
	if err != nil || count != rapidApprovalLimit {
		return err
	}
	slog.WarnContext(ctx, "rapid approvals", "person", personId, "lastMinute", count)
	_, err = events.Append(ctx, tx, events.Event{
		Type: "proposal.rapid_approvals", SubjectId: personId, Payload: map[string]any{"lastMinute": count},
	})
	return err
}

// RiskAtApproval is the risk an approval of the proposal, unedited, runs
// at under the action as it is now: the proposal's own, or the action's
// current one if that is higher.
func RiskAtApproval(action Action, proposal Proposal) string {
	if current := currentRisk(action, proposal.Payload); mandates.RiskRank(current) > mandates.RiskRank(proposal.Risk) {
		return current
	}
	return proposal.Risk
}

// currentRisk is the risk of a payload under the action as it is now.
func currentRisk(action Action, payload json.RawMessage) string {
	if action.Assess != nil {
		return action.Assess(payload)
	}
	return action.Risk
}

// keyPattern is what an idempotency key looks like: printable ASCII
// without spaces. The table holds the same pattern.
var keyPattern = regexp.MustCompile(`^[!-~]{1,200}$`)

// repeated finds the proposal an agent already made under a key. A key
// that was used for something else is a conflict.
func (s *Service) repeated(ctx context.Context, agentId uuid.UUID, key string, request []byte) (Proposal, bool, error) {
	var first Proposal
	found := false
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := repo.New(tx).GetByIdempotencyKey(ctx, repo.GetByIdempotencyKeyParams{AgentId: agentId, IdempotencyKey: key})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		first, err = sameRequest(row, request)
		return err
	})
	if err != nil {
		return Proposal{}, false, fault.FromDb(err)
	}
	return first, found, nil
}

// sameRequest returns the proposal when it was made from this request.
// The key names one request: the same key with something else in it is a
// mistake the agent should hear about, not an old answer to a new
// question. The comparison is with what was asked for, so a repeat is
// recognised after a person has edited the proposal.
func sameRequest(row repo.MandateProposal, request []byte) (Proposal, error) {
	if !bytes.Equal(row.RequestHash, request) {
		return Proposal{}, fault.New(fault.Conflict, fmt.Sprintf(
			"this idempotency key was already used, at %s, for proposal %s (%s) with another payload. If you meant to retry that request, read it with get_proposal and its proposal_id instead of proposing again; if this is a new request, use a new key that includes your run id",
			row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"), row.Id, row.Action))
	}
	return toProposal(row), nil
}

// Bounds on the numbers in a payload, once spelled out without exponents
// as the canonical form writes them: no number may be longer than
// maxNumberLength, and all of them together no longer than
// maxNumbersLength. Real payloads are far below both.
const (
	maxNumberLength  = 1000
	maxNumbersLength = 1 << 20
)

// checkNumbers refuses a payload whose numbers would take more to spell
// out than the payload itself suggests, such as 1e-999999999, or thousands
// of 1e-16383, before anything spells them out: the payload is the
// agent's, and it must not decide how much memory its numbers take.
func checkNumbers(payload json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	total := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fault.New(fault.InvalidArgument, "the payload must be a JSON object")
		}
		number, ok := token.(json.Number)
		if !ok {
			continue
		}
		length := events.PlainLength(string(number))
		if length < 0 || length > maxNumberLength {
			return fault.New(fault.InvalidArgument, "the payload holds a number too large or too precise to keep: "+truncate(string(number), 40))
		}
		if total += length; total > maxNumbersLength {
			return fault.New(fault.InvalidArgument, "the payload's numbers are too many or too long")
		}
	}
}

func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}

// requestHash identifies what an agent asked for: the action and the
// payload as it sent them, whatever the order of the fields or the
// spacing. Numbers keep every digit, so two payloads that differ only far
// down a long number are two requests.
func requestHash(action string, payload json.RawMessage) ([]byte, error) {
	if !json.Valid(payload) {
		return nil, errors.New("proposals: the payload is not JSON")
	}
	canonical, err := events.Canonical(map[string]any{"action": action, "payload": payload})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return sum[:], nil
}

// stringsOf collects every string in a JSON value, keys included.
func stringsOf(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, stringsOf(item)...)
		}
		return out
	case map[string]any:
		var out []string
		for key, item := range v {
			out = append(out, key)
			out = append(out, stringsOf(item)...)
		}
		return out
	}
	return nil
}

func toProposal(row repo.MandateProposal) Proposal {
	result := Proposal{
		Id: row.Id, AgentId: row.AgentId, RunId: deref(row.RunId), Action: row.Action, Risk: row.Risk, Title: row.Title,
		Payload: json.RawMessage(row.Payload), ProposedPayload: json.RawMessage(row.ProposedPayload), CostUnits: row.CostUnits, Flags: slices.Clone(row.Flags),
		IdempotencyKey: row.IdempotencyKey, Status: row.Status, Edited: row.Edited, DecidedBy: row.DecidedBy,
		DecisionNote: deref(row.DecisionNote), CreatedAt: row.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if result.Flags == nil {
		result.Flags = []string{}
	}
	if row.DecidedAt != nil {
		at := row.DecidedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		result.DecidedAt = &at
	}
	if len(row.ExecutionBasis) > 0 {
		var basis ExecutionBasis
		if err := json.Unmarshal(row.ExecutionBasis, &basis); err != nil {
			slog.Error("proposal has an unreadable execution basis", "proposal", row.Id, "error", err)
		} else {
			result.ExecutionBasis = &basis
		}
	}
	return result
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
