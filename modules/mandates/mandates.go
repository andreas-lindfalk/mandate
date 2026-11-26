// Package mandates holds the mandates: standing permissions, as rows, that let an
// agent act without a person. A mandate names the agent (or all), the
// actions, the highest risk, a budget per calendar month, a reference to the
// policy document it rests on (recorded, not checked), and until when it holds, and it may narrow all that
// with a condition on the content of a proposal. An owner grants it,
// nothing changes it, and it is revoked once. High risk is never delegated.
package mandates

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates/internal/repo"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"

	maxDays        = 366
	maxBudgetCount = 1_000_000_000
)

// RiskRank orders the risk levels; -1 for an unknown one.
func RiskRank(risk string) int {
	switch risk {
	case RiskLow:
		return 0
	case RiskMedium:
		return 1
	case RiskHigh:
		return 2
	}
	return -1
}

type Mandate struct {
	Id          uuid.UUID
	AgentId     *uuid.UUID
	Actions     []string
	MaxRisk     string
	BudgetCount int64
	PolicyRef   string
	Department  string
	GrantedBy   *uuid.UUID
	ValidFrom   time.Time
	ValidUntil  time.Time
	RevokedAt   *time.Time
	RevokedBy   *uuid.UUID
	CreatedAt   time.Time
	// Condition is the CEL condition the mandate was granted with, or empty.
	Condition string
	// plan is the condition compiled, as stored with the mandate.
	plan Plan
}

type Input struct {
	// AgentId nil means every agent of the tenant.
	AgentId *uuid.UUID
	// Actions are keys (tickets.create), prefixes (tickets.*) or *.
	Actions []string
	// MaxRisk is low or medium. High is never delegated.
	MaxRisk string
	// BudgetCount is how many executions it may allow per UTC calendar month.
	BudgetCount int64
	// PolicyRef names the policy document the mandate rests on. It is recorded
	// with every decision the mandate allows; the document itself is not
	// stored or checked.
	PolicyRef string
	// Days the mandate holds from now, at most 366.
	Days int
	// Department is an optional label.
	Department string
	// Condition optionally narrows the mandate by the content of a proposal,
	// such as payload.amount <= 500. See CompileCondition for what it may say.
	Condition string
}

type Service struct {
	pool *pgxpool.Pool
	// builtin lists the actions an embedded host registered in code, by key,
	// with their payload schema or nil.
	builtin func() map[string]json.RawMessage
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// SetBuiltin tells the service which actions exist only as code, so a
// condition is checked against their schemas too. The kernel wires it.
func (s *Service) SetBuiltin(builtin func() map[string]json.RawMessage) {
	s.builtin = builtin
}

// Grant creates a mandate in the tenant in scope. Only an owner may, which
// the database enforces.
func (s *Service) Grant(ctx context.Context, input Input) (Mandate, error) {
	tenantId, ok := actor.TenantFrom(ctx)
	if !ok {
		return Mandate{}, fault.New(fault.InvalidArgument, "no tenant in scope")
	}
	input.PolicyRef = strings.TrimSpace(input.PolicyRef)
	input.Department = strings.TrimSpace(input.Department)
	input.Condition = strings.TrimSpace(input.Condition)
	switch {
	case utf8.RuneCountInString(input.Department) > 100:
		return Mandate{}, fault.New(fault.InvalidArgument, "the department can be at most 100 characters")
	case len(input.Actions) == 0 || len(input.Actions) > 50:
		return Mandate{}, fault.New(fault.InvalidArgument, "a mandate names 1 to 50 actions")
	case !ValidPatterns(input.Actions):
		return Mandate{}, fault.New(fault.InvalidArgument, "actions are keys like tickets.create, prefixes like tickets.*, or *")
	case input.MaxRisk != RiskLow && input.MaxRisk != RiskMedium:
		return Mandate{}, fault.New(fault.InvalidArgument, "the risk limit is low or medium; high is never delegated")
	case input.BudgetCount < 1 || input.BudgetCount > maxBudgetCount:
		return Mandate{}, fault.New(fault.InvalidArgument, "the budget is 1 to 1,000,000,000 executions a month")
	case input.PolicyRef == "" || utf8.RuneCountInString(input.PolicyRef) > 200:
		return Mandate{}, fault.New(fault.InvalidArgument, "a mandate rests on a policy document: give its reference, 1 to 200 characters")
	case input.Days < 1 || input.Days > maxDays:
		return Mandate{}, fault.New(fault.InvalidArgument, "a mandate holds for 1 to 366 days")
	}
	var condition *string
	var planJson []byte
	var plan Plan
	if input.Condition != "" {
		var err error
		if plan, err = CompileCondition(input.Condition); err != nil {
			return Mandate{}, err
		}
		if planJson, err = json.Marshal(plan); err != nil {
			return Mandate{}, err
		}
		condition = &input.Condition
	}
	id := uuid.New()
	var result Mandate
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := repo.New(tx)
		if input.AgentId != nil {
			live, err := q.AgentState(ctx, *input.AgentId)
			if errors.Is(err, pgx.ErrNoRows) {
				return fault.New(fault.NotFound, "no such agent in this tenant")
			}
			if err != nil {
				return err
			}
			if !live {
				return fault.New(fault.InvalidArgument, "the agent is revoked")
			}
		}
		if condition != nil {
			schemas, err := s.coveredSchemas(ctx, q, input.Actions)
			if err != nil {
				return err
			}
			if err := plan.CheckAgainstSchemas(input.Actions, schemas); err != nil {
				return err
			}
		}
		var department *string
		if input.Department != "" {
			department = &input.Department
		}
		if err := q.InsertMandate(ctx, repo.InsertMandateParams{
			Id: id, TenantId: tenantId, AgentId: input.AgentId, Actions: input.Actions, MaxRisk: input.MaxRisk,
			BudgetCount: input.BudgetCount, PolicyRef: input.PolicyRef, Department: department,
			// Days of 24 hours: a daylight saving change never makes 366
			// days an hour longer than the database's check allows.
			Lasts:     pgtype.Interval{Microseconds: (time.Duration(input.Days) * 24 * time.Hour).Microseconds(), Valid: true},
			Condition: condition, ConditionPlan: planJson,
		}); err != nil {
			return err
		}
		row, err := q.GetMandate(ctx, id)
		if err != nil {
			return err
		}
		result = toMandate(row)
		_, err = events.Append(ctx, tx, events.Event{
			Type: "mandate.granted", SubjectId: id,
			Payload: map[string]any{
				"agentId": input.AgentId, "actions": input.Actions, "maxRisk": input.MaxRisk, "budgetCount": input.BudgetCount,
				"policyRef": input.PolicyRef, "department": input.Department, "validUntil": result.ValidUntil.UTC().Format(time.RFC3339),
				"condition": input.Condition,
			},
		})
		return err
	})
	return result, fault.FromDb(err)
}

// coveredSchemas returns the payload schema of every action the patterns
// cover now, defined or builtin, with nil for one that has none. A tenant's
// definition takes precedence over the code, and a removed one hides it.
func (s *Service) coveredSchemas(ctx context.Context, q *repo.Queries, patterns []string) ([]json.RawMessage, error) {
	rows, err := q.CoveredActions(ctx, patterns)
	if err != nil {
		return nil, err
	}
	defined := map[string]bool{}
	var schemas []json.RawMessage
	for _, row := range rows {
		defined[row.Key] = true
		if !row.Removed {
			schemas = append(schemas, row.Schema)
		}
	}
	if s.builtin != nil {
		for key, schema := range s.builtin() {
			if !defined[key] && Covers(patterns, key) {
				schemas = append(schemas, schema)
			}
		}
	}
	return schemas, nil
}

// Revoke ends a mandate for good. Only an owner may; a mandate revoked
// already is a Conflict.
func (s *Service) Revoke(ctx context.Context, id uuid.UUID) error {
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := db.RequireOwner(ctx, tx); err != nil {
			return err
		}
		q := repo.New(tx)
		if _, err := q.GetMandate(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "mandate not found")
		} else if err != nil {
			return err
		}
		changed, err := q.RevokeMandate(ctx, id)
		if err != nil {
			return err
		}
		if changed == 0 {
			return fault.New(fault.Conflict, "the mandate is already revoked")
		}
		_, err = events.Append(ctx, tx, events.Event{Type: "mandate.revoked", SubjectId: id, Payload: map[string]any{}})
		return err
	})
	return fault.FromDb(err)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Mandate, error) {
	var result Mandate
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := repo.New(tx).GetMandate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault.New(fault.NotFound, "mandate not found")
		}
		result = toMandate(row)
		return err
	})
	return result, err
}

// List returns the mandates that hold now, or all of them.
func (s *Service) List(ctx context.Context, includeEnded bool) ([]Mandate, error) {
	var result []Mandate
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).ListMandates(ctx, includeEnded)
		for _, row := range rows {
			result = append(result, toMandate(row))
		}
		return err
	})
	return result, err
}

// Covers says whether the mandate admits the action.
func (m Mandate) Covers(action string) bool {
	return Covers(m.Actions, action)
}

// ConditionHolds says whether the mandate's condition, if it has one, is
// true for the action and payload. A condition that cannot be evaluated
// does not hold.
func (m Mandate) ConditionHolds(action string, payload json.RawMessage) bool {
	return m.Condition == "" || m.plan.Holds(action, payload)
}

// Allows says whether the mandate admits the risk.
func (m Mandate) Allows(risk string) bool {
	rank := RiskRank(risk)
	return rank >= 0 && rank <= RiskRank(m.MaxRisk)
}

// Covers mirrors mandate.covers in the database.
func Covers(patterns []string, action string) bool {
	for _, p := range patterns {
		if p == "*" || p == action {
			return true
		}
		if strings.HasSuffix(p, ".*") && strings.HasPrefix(action, p[:len(p)-1]) {
			return true
		}
	}
	return false
}

// ValidPatterns mirrors mandate.valid_action_patterns in the database.
func ValidPatterns(patterns []string) bool {
	for _, p := range patterns {
		if p == "*" || ValidKey(p) || (strings.HasSuffix(p, ".*") && ValidKey(p[:len(p)-2])) {
			continue
		}
		return false
	}
	return len(patterns) > 0
}

// ValidKey mirrors mandate.valid_action_key in the database.
func ValidKey(key string) bool {
	if key == "" || len(key) > 100 {
		return false
	}
	for _, part := range strings.Split(key, ".") {
		if part == "" || part[0] < 'a' || part[0] > 'z' {
			return false
		}
		for _, r := range part {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
				return false
			}
		}
	}
	return true
}

// Period is the budget period that holds the instant: the UTC calendar
// month. The database counts the spend with its own clock
// (mandate.spent_count); this is the same rule, for callers that reason
// about it in Go.
func Period(at time.Time) (start, end time.Time) {
	year, month, _ := at.UTC().Date()
	start = time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// Resolution is what the mandates say about executing a proposal now: the
// mandate that allows it with its spend so far, or the reason none does.
type Resolution struct {
	Mandate *Mandate
	// Spent is how many executions the chosen mandate has allowed this period.
	Spent int64
	// Reason is empty when a mandate allows it, else one of the Skip reasons.
	Reason string
}

// Reasons a resolution says no.
const (
	SkipNoMandate   = "no_mandate"
	SkipRiskAbove   = "risk_above_mandate"
	SkipBudgetSpent = "budget_spent"
	// SkipConditionUnmet: mandates cover the action at the risk, but the
	// condition of each is false or cannot be evaluated for the payload.
	SkipConditionUnmet = "condition_not_met"
)

// Resolve finds, inside the caller's transaction, a live mandate for the
// agent that covers the action at the risk, whose condition holds for the
// payload, and that still has room in its budget. The live mandates that
// cover the action are locked, so two executions under one mandate count the
// budget one after the other. The most permissive mandate with
// room is chosen. The spend is what the database counts, so the basis
// recorded here is exactly what the execution trigger will check.
func (s *Service) Resolve(ctx context.Context, tx pgx.Tx, agentId uuid.UUID, action, risk string, payload json.RawMessage) (Resolution, error) {
	// Only the mandates that cover the action are read, and so locked: an
	// execution of another action, by this agent or another, never waits.
	rows, err := repo.New(tx).ListLiveForAgent(ctx, repo.ListLiveForAgentParams{AgentId: agentId, Action: action})
	if err != nil {
		return Resolution{}, err
	}
	var covering, atRisk, allowing []Mandate
	for _, row := range rows {
		m := toMandate(row)
		if !m.Covers(action) {
			continue
		}
		covering = append(covering, m)
		if !m.Allows(risk) {
			continue
		}
		atRisk = append(atRisk, m)
		if m.ConditionHolds(action, payload) {
			allowing = append(allowing, m)
		}
	}
	switch {
	case len(covering) == 0:
		return Resolution{Reason: SkipNoMandate}, nil
	case len(atRisk) == 0:
		return Resolution{Reason: SkipRiskAbove}, nil
	case len(allowing) == 0:
		return Resolution{Reason: SkipConditionUnmet}, nil
	}
	for _, m := range allowing {
		spent, err := repo.New(tx).SpentCount(ctx, m.Id)
		if err != nil {
			return Resolution{}, err
		}
		if spent < m.BudgetCount {
			m := m
			return Resolution{Mandate: &m, Spent: spent}, nil
		}
	}
	return Resolution{Reason: SkipBudgetSpent}, nil
}

// Live lists the mandates that hold now for the agent, by the database's
// clock: its own and those for every agent of the tenant.
func (s *Service) Live(ctx context.Context, agentId uuid.UUID) ([]Mandate, error) {
	var out []Mandate
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).ListLiveForAgentToRead(ctx, agentId)
		for _, row := range rows {
			out = append(out, toMandate(row))
		}
		return err
	})
	// Newest first, as the list of mandates shows them.
	slices.SortStableFunc(out, func(a, b Mandate) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, err
}

// Unmet explains a proposal left for a person because no mandate's
// condition held: for each live mandate with a condition that covers the
// agent, the action and the risk, what in the payload stood in the way.
// It reads the mandates live now by the database's clock, as Resolve does,
// which may differ from when the proposal was tried.
func (s *Service) Unmet(ctx context.Context, agentId uuid.UUID, action, risk string, payload json.RawMessage) ([]string, error) {
	var rows []repo.MandateMandate
	if err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		rows, err = repo.New(tx).ListLiveForAgentToRead(ctx, agentId)
		return err
	}); err != nil {
		return nil, err
	}
	var out []string
	for _, row := range rows {
		m := toMandate(row)
		if m.Condition == "" || !m.Covers(action) || !m.Allows(risk) {
			continue
		}
		for _, line := range m.plan.Unmet(action, payload) {
			out = append(out, "mandate "+m.Id.String()[:8]+": "+line)
		}
	}
	return out, nil
}

// SpentFor is Spent for many mandates in one query.
func (s *Service) SpentFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int64, error) {
	result := map[uuid.UUID]int64{}
	if len(ids) == 0 {
		return result, nil
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).SpentCountFor(ctx, ids)
		for _, row := range rows {
			result[row.Id] = row.Spent
		}
		return err
	})
	return result, err
}

// Spent is how many executions a mandate has allowed in the current period.
func (s *Service) Spent(ctx context.Context, mandateId uuid.UUID) (int64, error) {
	var spent int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		spent, err = repo.New(tx).SpentCount(ctx, mandateId)
		return err
	})
	return spent, err
}

func toMandate(row repo.MandateMandate) Mandate {
	m := Mandate{
		Id: row.Id, AgentId: row.AgentId, Actions: slices.Clone(row.Actions), MaxRisk: row.MaxRisk, BudgetCount: row.BudgetCount,
		PolicyRef: row.PolicyRef, GrantedBy: row.GrantedBy, ValidFrom: row.ValidFrom, ValidUntil: row.ValidUntil,
		RevokedAt: row.RevokedAt, RevokedBy: row.RevokedBy, CreatedAt: row.CreatedAt,
	}
	if row.Department != nil {
		m.Department = *row.Department
	}
	if row.Condition != nil {
		m.Condition = *row.Condition
		// A plan that does not read is left as the zero Plan, which never
		// holds. Decoding into the mandate's own plan could leave part of it.
		var plan Plan
		if err := json.Unmarshal(row.ConditionPlan, &plan); err != nil {
			slog.Error("a mandate's condition plan does not read", "mandate", row.Id, "error", err)
		} else {
			m.plan = plan
		}
	}
	return m
}
