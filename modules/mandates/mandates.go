// Package mandates holds the mandates: the policy, as rows, that lets an
// agent act without a person. A mandate names the agent (or all), the
// actions, the highest risk, a budget per calendar month, the adopted policy
// document it rests on, and until when it holds. An owner grants it, nothing
// changes it, and it is revoked once. High risk is never delegated.
package mandates

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	maxBudgetUnits = 1_000_000_000_000
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
	BudgetUnits int64
	PolicyRef   string
	Department  string
	GrantedBy   *uuid.UUID
	ValidFrom   time.Time
	ValidUntil  time.Time
	RevokedAt   *time.Time
	RevokedBy   *uuid.UUID
	CreatedAt   time.Time
}

type Input struct {
	// AgentId nil means every agent of the tenant.
	AgentId *uuid.UUID
	// Actions are keys (tickets.create), prefixes (tickets.*) or *.
	Actions []string
	// MaxRisk is low or medium. High is never delegated.
	MaxRisk string
	// BudgetUnits per calendar month, in the host's cost units.
	BudgetUnits int64
	// PolicyRef names the adopted policy document the mandate rests on.
	PolicyRef string
	// Days the mandate holds from now, at most 366.
	Days int
	// Department is an optional label.
	Department string
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// SetNow replaces the clock, for tests of validity and the month boundary.
func (s *Service) SetNow(now func() time.Time) {
	s.now = now
}

// Now is the clock the service uses.
func (s *Service) Now() time.Time {
	return s.now()
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
	switch {
	case utf8.RuneCountInString(input.Department) > 100:
		return Mandate{}, fault.New(fault.InvalidArgument, "the department can be at most 100 characters")
	case len(input.Actions) == 0 || len(input.Actions) > 50:
		return Mandate{}, fault.New(fault.InvalidArgument, "a mandate names 1 to 50 actions")
	case !ValidPatterns(input.Actions):
		return Mandate{}, fault.New(fault.InvalidArgument, "actions are keys like tickets.create, prefixes like tickets.*, or *")
	case input.MaxRisk != RiskLow && input.MaxRisk != RiskMedium:
		return Mandate{}, fault.New(fault.InvalidArgument, "the risk limit is low or medium; high is never delegated")
	case input.BudgetUnits <= 0 || input.BudgetUnits > maxBudgetUnits:
		return Mandate{}, fault.New(fault.InvalidArgument, "the budget is 1 or more units per month")
	case input.PolicyRef == "" || utf8.RuneCountInString(input.PolicyRef) > 200:
		return Mandate{}, fault.New(fault.InvalidArgument, "a mandate rests on a policy document: give its reference")
	case input.Days < 1 || input.Days > maxDays:
		return Mandate{}, fault.New(fault.InvalidArgument, "a mandate holds for 1 to 366 days")
	}
	// Validity is counted in UTC days, so a daylight saving change never
	// makes 366 days one hour too long for the database's check.
	now := s.now().UTC()
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
		var department *string
		if input.Department != "" {
			department = &input.Department
		}
		if err := q.InsertMandate(ctx, repo.InsertMandateParams{
			Id: id, TenantId: tenantId, AgentId: input.AgentId, Actions: input.Actions, MaxRisk: input.MaxRisk,
			BudgetUnits: input.BudgetUnits, PolicyRef: input.PolicyRef, Department: department, ValidFrom: now, ValidUntil: now.AddDate(0, 0, input.Days),
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
				"agentId": input.AgentId, "actions": input.Actions, "maxRisk": input.MaxRisk, "budgetUnits": input.BudgetUnits,
				"policyRef": input.PolicyRef, "department": input.Department, "validUntil": result.ValidUntil.UTC().Format(time.RFC3339),
			},
		})
		return err
	})
	return result, fault.FromDb(err)
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
// (mandate.spent_units); this is the same rule, for callers that reason
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
	// SpentUnits is what the chosen mandate's executions have cost this period.
	SpentUnits int64
	// Reason is empty when a mandate allows it, else one of the Skip reasons.
	Reason string
}

// Reasons a resolution says no.
const (
	SkipNoMandate   = "no_mandate"
	SkipRiskAbove   = "risk_above_mandate"
	SkipBudgetSpent = "budget_spent"
)

// Resolve finds, inside the caller's transaction, a live mandate for the
// agent that covers the action at the risk and still has budget for the
// cost. The live mandates are locked, so two executions under one mandate
// count the budget one after the other. The most permissive mandate with
// room is chosen. The spend is what the database counts, so the basis
// recorded here is exactly what the execution trigger will check.
func (s *Service) Resolve(ctx context.Context, tx pgx.Tx, agentId uuid.UUID, action, risk string, costUnits int64) (Resolution, error) {
	rows, err := repo.New(tx).ListLiveForAgent(ctx, repo.ListLiveForAgentParams{AgentId: agentId, At: s.now()})
	if err != nil {
		return Resolution{}, err
	}
	var covering, allowing []Mandate
	for _, row := range rows {
		m := toMandate(row)
		if !m.Covers(action) {
			continue
		}
		covering = append(covering, m)
		if m.Allows(risk) {
			allowing = append(allowing, m)
		}
	}
	switch {
	case len(covering) == 0:
		return Resolution{Reason: SkipNoMandate}, nil
	case len(allowing) == 0:
		return Resolution{Reason: SkipRiskAbove}, nil
	}
	for _, m := range allowing {
		spent, err := repo.New(tx).SpentUnits(ctx, m.Id)
		if err != nil {
			return Resolution{}, err
		}
		if spent+costUnits <= m.BudgetUnits {
			m := m
			return Resolution{Mandate: &m, SpentUnits: spent}, nil
		}
	}
	return Resolution{Reason: SkipBudgetSpent}, nil
}

// SpentFor is Spent for many mandates in one query.
func (s *Service) SpentFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int64, error) {
	result := map[uuid.UUID]int64{}
	if len(ids) == 0 {
		return result, nil
	}
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := repo.New(tx).SpentUnitsFor(ctx, ids)
		for _, row := range rows {
			result[row.Id] = row.Spent
		}
		return err
	})
	return result, err
}

// Spent is what the executions under a mandate have cost in the current period.
func (s *Service) Spent(ctx context.Context, mandateId uuid.UUID) (int64, error) {
	var spent int64
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		spent, err = repo.New(tx).SpentUnits(ctx, mandateId)
		return err
	})
	return spent, err
}

func toMandate(row repo.MandateMandate) Mandate {
	m := Mandate{
		Id: row.Id, AgentId: row.AgentId, Actions: slices.Clone(row.Actions), MaxRisk: row.MaxRisk, BudgetUnits: row.BudgetUnits,
		PolicyRef: row.PolicyRef, GrantedBy: row.GrantedBy, ValidFrom: row.ValidFrom, ValidUntil: row.ValidUntil,
		RevokedAt: row.RevokedAt, RevokedBy: row.RevokedBy, CreatedAt: row.CreatedAt,
	}
	if row.Department != nil {
		m.Department = *row.Department
	}
	return m
}
