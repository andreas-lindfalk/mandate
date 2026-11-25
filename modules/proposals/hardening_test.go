package proposals_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// HardeningSuite tests the rules the database holds by itself, with the
// Go checks bypassed, and the races the locking exists for.
type HardeningSuite struct {
	IntegrationSuite
}

func TestHardeningSuite(t *testing.T) {
	suite.Run(t, new(HardeningSuite))
}

func (s *HardeningSuite) define(ctx context.Context, key, risk string, mayExecute bool) {
	s.T().Helper()
	_, err := s.W.Kernel.Actions.Define(ctx, actions.Input{
		Key: key, Risk: risk, MayExecute: mayExecute, Connector: actions.ConnectorWebhook, Url: "https://connector.example/hooks",
		Schema: json.RawMessage(`{"type":"object"}`),
	})
	s.Require().NoError(err)
}

const insertProposal = `INSERT INTO mandate.proposals (id, tenant_id, agent_id, action, risk, title, payload, flags, idempotency_key, request_hash, rationale)
	VALUES ($1, $2, $3, $4, $5, 'forged', '{}', '{}', gen_random_uuid()::text, sha256('forged'), 'Test rationale.')`

func (s *HardeningSuite) Test_AnAgentCannotUnderstateRisk() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.define(t.Owner, "wire.send", mandates.RiskHigh, false)
	s.refused(t.Agent, insertProposal, uuid.New(), t.Id, t.AgentId, "wire.send", "low")
	s.Require().NoError(s.exec(t.Agent, insertProposal, uuid.New(), t.Id, t.AgentId, "wire.send", "high"))

	s.Require().NoError(s.W.Kernel.Actions.Remove(t.Owner, "wire.send"))
	s.refused(t.Agent, insertProposal, uuid.New(), t.Id, t.AgentId, "wire.send", "high")

	s.define(t.Owner, "notes.add", mandates.RiskLow, true)
	s.Require().NoError(s.W.Kernel.Agents.Revoke(t.Owner, t.AgentId))
	s.refused(t.Agent, insertProposal, uuid.New(), t.Id, t.AgentId, "notes.add", "low")
}

// executeDirectly tries the status change the way Execute does, without
// Execute: as the system, inside a transaction that names the proposal.
func (s *HardeningSuite) executeDirectly(ctx context.Context, id uuid.UUID, basis map[string]any) error {
	raw, err := json.Marshal(basis)
	s.Require().NoError(err)
	return db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('mandate.executing_proposal', $1, true)`, id.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE mandate.proposals SET status = 'executed', execution_basis = $2 WHERE id = $1`, id, raw)
		return err
	})
}

func (s *HardeningSuite) Test_TheDatabaseRefusesWhatOnlyGoUsedToCheck() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.define(t.Owner, "notes.add", mandates.RiskLow, false)
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "notes.add", Title: "note", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	basis := map[string]any{"mandateId": m.Id.String(), "policyRef": "P", "maxRisk": "low", "budgets": []any{map[string]any{"kind": "count", "limit": 10, "spent": 0}}}

	// The action is defined as one a person always decides.
	err = s.executeDirectly(t.System, p.Id, basis)
	s.code(fault.FromDb(err), fault.PermissionDenied)

	// Once it may execute, the same statement is accepted: the check above
	// was the action's, not something else in the basis.
	s.define(t.Owner, "notes.add", mandates.RiskLow, true)
	upper := map[string]any{"mandateId": "{" + m.Id.String() + "}", "policyRef": "P", "maxRisk": "low", "budgets": []any{map[string]any{"kind": "count", "limit": 10, "spent": 0}}}
	err = s.executeDirectly(t.System, p.Id, upper)
	s.code(fault.FromDb(err), fault.PermissionDenied)
	s.T().Log("a mandate id that is not in canonical form is refused, so the spend cannot be hidden:", err)
	s.Require().NoError(s.executeDirectly(t.System, p.Id, basis))
	spent, err := s.W.Kernel.Mandates.Spent(t.Owner, m.Id)
	s.Require().NoError(err)
	s.Require().Equal(int64(1), spent)
}

// The database evaluates a mandate's condition itself: an execution of a
// payload the condition does not allow is refused, and so is a basis that
// does not cite the condition as granted.
func (s *HardeningSuite) Test_TheDatabaseHoldsAnExecutionToTheCondition() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.define(t.Owner, "notes.add", mandates.RiskLow, true)
	const condition = `payload.amount <= 500`
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30, Condition: condition})
	s.Require().NoError(err)
	propose := func(payload string) uuid.UUID {
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "notes.add", Title: "note", Payload: json.RawMessage(payload)})
		s.Require().NoError(err)
		return p.Id
	}
	basis := func(condition any) map[string]any {
		b := map[string]any{"mandateId": m.Id.String(), "policyRef": "P", "maxRisk": "low", "budgets": []any{map[string]any{"kind": "count", "limit": 10, "spent": 0}}}
		if condition != nil {
			b["condition"] = condition
		}
		return b
	}
	for payload, why := range map[string]string{
		`{"amount": 600}`:   "above the limit",
		`{"amount": "400"}`: "a string is not a number",
		`{}`:                "no amount at all",
	} {
		err := s.executeDirectly(t.System, propose(payload), basis(condition))
		s.code(fault.FromDb(err), fault.PermissionDenied)
		s.T().Log("refused, "+why+":", err)
	}
	small := propose(`{"amount": 400}`)
	s.code(fault.FromDb(s.executeDirectly(t.System, small, basis(nil))), fault.PermissionDenied)
	s.code(fault.FromDb(s.executeDirectly(t.System, small, basis(`payload.amount <= 5000`))), fault.PermissionDenied)
	s.Require().NoError(s.executeDirectly(t.System, small, basis(condition)))
	executed, err := s.W.Kernel.Proposals.Get(t.Owner, small)
	s.Require().NoError(err)
	s.Require().Equal(condition, executed.ExecutionBasis.Condition)
}

// A payload key that a receiver could read as another is refused by the
// service and, without it, by the database: a condition checked on one key
// could otherwise be carried out on the other.
func (s *HardeningSuite) Test_APayloadKeyIsReadOneWayOnly() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.define(t.Owner, "notes.add", mandates.RiskLow, true)
	_, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{
		IdempotencyKey: uuid.NewString(), Action: "notes.add", Title: "x", Rationale: "Why.", Payload: json.RawMessage(`{"amountCents":100,"amountcents":999999}`),
	})
	s.code(err, fault.InvalidArgument)
	s.Require().Contains(err.Error(), "amountcents")
	insert := `INSERT INTO mandate.proposals (id, tenant_id, agent_id, action, risk, title, payload, idempotency_key, request_hash, rationale)
		VALUES (gen_random_uuid(), $1, $2, 'notes.add', 'low', 'x', $3::jsonb, gen_random_uuid()::text, sha256('x'), 'Why.')`
	for _, payload := range []string{
		`{"amountCents":100,"amountcents":999999}`,
		`{"order":{"Id":"a","ID":"b"}}`,
		`{"items":[{"x":1,"X":2}]}`,
		`{"ſtatus":"paid"}`,
	} {
		s.invalid(t.Agent, insert, t.Id, t.AgentId, payload)
	}
	s.Require().NoError(s.exec(t.Agent, insert, t.Id, t.AgentId, `{"a":{"b":1},"A2":[{"b":2}],"note":"amountCents amountcents"}`))

	// A proposal stored before the rule, with such a payload, can still be
	// decided: the rule holds a payload when it is written, not every update.
	legacy := uuid.New()
	_, err = s.W.Pg.Owner.Exec(context.Background(), `
		BEGIN;
		SET LOCAL session_replication_role = replica;
		INSERT INTO mandate.proposals (id, tenant_id, agent_id, action, risk, title, payload, proposed_payload, expires_at, deliver_within, idempotency_key, request_hash, rationale)
		VALUES ('`+legacy.String()+`', '`+t.Id.String()+`', '`+t.AgentId.String()+`', 'notes.add', 'low', 'old', '{"a":1,"A":2}', '{"a":1,"A":2}', now() + interval '7 days', interval '1 hour', 'legacy', sha256('x'), 'Why.');
		COMMIT;`)
	s.Require().NoError(err)
	_, err = s.W.Kernel.Proposals.Reject(t.Reviewer, legacy, "Stored before the rule.")
	s.Require().NoError(err)
}

// The budget and the agent's limits are counted after a lock, which holds
// only under read committed; another isolation is refused, not trusted.
func (s *HardeningSuite) Test_CountsAreMadeUnderReadCommittedOnly() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.define(t.Owner, "notes.add", mandates.RiskLow, true)
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{IdempotencyKey: uuid.NewString(), Action: "notes.add", Title: "x", Payload: json.RawMessage(`{}`), Rationale: "Why."})
	s.Require().NoError(err)
	basis, err := json.Marshal(map[string]any{"mandateId": m.Id.String(), "policyRef": "P", "maxRisk": "low", "budgets": []any{map[string]any{"kind": "count", "limit": 10, "spent": 0}}})
	s.Require().NoError(err)
	// The scope is set by hand, as db.InTx would, in a transaction begun at
	// another isolation level.
	at := func(level pgx.TxIsoLevel, kind string, actorId uuid.UUID, statements ...func(tx pgx.Tx) error) error {
		ctx := context.Background()
		tx, err := s.W.Pg.App.BeginTx(ctx, pgx.TxOptions{IsoLevel: level})
		s.Require().NoError(err)
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SELECT set_config('mandate.tenant_id', $1, true), set_config('mandate.actor_kind', $2, true), set_config('mandate.actor_id', $3, true)`,
			t.Id.String(), kind, actorId.String()); err != nil {
			return err
		}
		for _, statement := range statements {
			if err := statement(tx); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	err = at(pgx.RepeatableRead, "system", uuid.Nil, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `SELECT set_config('mandate.executing_proposal', $1, true)`, p.Id.String()); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `UPDATE mandate.proposals SET status = 'executed', execution_basis = $2 WHERE id = $1`, p.Id, basis)
		return err
	})
	s.Require().ErrorContains(err, "read committed")
	err = at(pgx.Serializable, "agent", t.AgentId, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), insertProposal, uuid.New(), t.Id, t.AgentId, "notes.add", "low")
		return err
	})
	s.Require().ErrorContains(err, "read committed")
	s.Require().NoError(at(pgx.ReadCommitted, "agent", t.AgentId, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), insertProposal, uuid.New(), t.Id, t.AgentId, "notes.add", "low")
		return err
	}), "the same insert under read committed is taken")
	s.Require().NoError(s.executeDirectly(t.System, p.Id, map[string]any{"mandateId": m.Id.String(), "policyRef": "P", "maxRisk": "low", "budgets": []any{map[string]any{"kind": "count", "limit": 10, "spent": 0}}}))
}

// The trigger takes the mandate's lock itself: two executions that skip
// the service entirely still count the budget one after the other.
func (s *HardeningSuite) Test_TheTriggerLocksTheBudgetByItself() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.define(t.Owner, "notes.add", mandates.RiskLow, true)
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 1, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	basis := map[string]any{"mandateId": m.Id.String(), "policyRef": "P", "maxRisk": "low", "budgets": []any{map[string]any{"kind": "count", "limit": 1, "spent": 0}}}
	const racers = 6
	ids := make([]uuid.UUID, racers)
	for i := range ids {
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "notes.add", Title: "note", Payload: json.RawMessage(`{}`)})
		s.Require().NoError(err)
		ids[i] = p.Id
	}
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.executeDirectly(t.System, id, basis)
		}()
	}
	wg.Wait()
	accepted := 0
	for _, err := range errs {
		if err == nil {
			accepted++
		} else {
			s.code(fault.FromDb(err), fault.PermissionDenied)
		}
	}
	s.Require().Equal(1, accepted, "a budget of one admits one execution")
	spent, err := s.W.Kernel.Mandates.Spent(t.Owner, m.Id)
	s.Require().NoError(err)
	s.Require().Equal(int64(1), spent)
}

func (s *HardeningSuite) Test_ARemovedActionsProposalsCanStillBeRejected() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.define(t.Owner, "notes.add", mandates.RiskLow, false)
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "notes.add", Title: "note", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.Actions.Remove(t.Owner, "notes.add"))
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	s.code(err, fault.InvalidArgument)
	rejected, err := s.W.Kernel.Proposals.Reject(t.Owner, p.Id, "the action is gone")
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusRejected, rejected.Status)
}

func (s *HardeningSuite) Test_ParallelExecutionsNeverOverspend() {
	t := s.W.NewTenant(s.T(), "Acme")
	const budget, proposed = 3, 8
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"test.*"}, MaxRisk: mandates.RiskLow, BudgetCount: budget, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	ids := make([]uuid.UUID, 0, proposed)
	for i := 0; i < proposed; i++ {
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "test.low", Title: "parallel", Payload: json.RawMessage(`{}`)})
		s.Require().NoError(err)
		ids = append(ids, p.Id)
	}
	var wg sync.WaitGroup
	errs := make([]error, proposed)
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.W.Kernel.ExecuteProposal(context.Background(), t.Id, id)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		s.Require().NoError(err)
	}
	executed := 0
	for _, id := range ids {
		p, err := s.W.Kernel.Proposals.Get(t.Owner, id)
		s.Require().NoError(err)
		if p.Status == proposals.StatusExecuted {
			executed++
		}
	}
	s.Require().Equal(budget, executed, "exactly the budget is executed, however the executions interleave")
	spent, err := s.W.Kernel.Mandates.Spent(t.Owner, m.Id)
	s.Require().NoError(err)
	s.Require().Equal(int64(budget), spent)
	s.verifyChain(t.Owner)
}

func (s *HardeningSuite) Test_ApproveAndExecuteRaceHasOneWinner() {
	t := s.W.NewTenant(s.T(), "Acme")
	_, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"test.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 100, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	for round := 0; round < 10; round++ {
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "test.low", Title: "race", Payload: json.RawMessage(`{}`)})
		s.Require().NoError(err)
		var wg sync.WaitGroup
		var approveErr, rejectErr, executeErr error
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, approveErr = s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
		}()
		go func() { defer wg.Done(); _, rejectErr = s.W.Kernel.Proposals.Reject(t.Reviewer, p.Id, "no") }()
		go func() { defer wg.Done(); executeErr = s.W.Kernel.ExecuteProposal(context.Background(), t.Id, p.Id) }()
		wg.Wait()
		s.Require().NoError(executeErr, "losing the race is not a failure of the job")
		after, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
		s.Require().NoError(err)
		s.Require().NotEqual(proposals.StatusPending, after.Status)
		wins := 0
		if approveErr == nil {
			wins++
			s.Require().Equal(proposals.StatusApproved, after.Status)
		} else {
			s.code(approveErr, fault.Conflict)
		}
		if rejectErr == nil {
			wins++
			s.Require().Equal(proposals.StatusRejected, after.Status)
		} else {
			s.code(rejectErr, fault.Conflict)
		}
		if after.Status == proposals.StatusExecuted {
			wins++
		}
		s.Require().Equal(1, wins, "one decision per proposal")
		decisions := 0
		for _, eventType := range s.eventTypes(t.Owner, p.Id) {
			if eventType == "proposal.approved" || eventType == "proposal.rejected" || eventType == "proposal.executed" {
				decisions++
			}
		}
		s.Require().Equal(1, decisions, "and one decision in the chain")
	}
	s.verifyChain(t.Owner)
}

// A payload is held to the action's schema as it is when the decision is
// made, not only as it was when the agent proposed.
func (s *HardeningSuite) Test_ADecisionHoldsThePayloadToTheSchemaOfToday() {
	t := s.W.NewTenant(s.T(), "Acme")
	strict := func() {
		s.T().Helper()
		_, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
			Key: "notes.add", Risk: mandates.RiskLow, MayExecute: true, Connector: actions.ConnectorWebhook, Url: "https://connector.example/hooks",
			Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["text"],"properties":{"text":{"type":"string"}}}`),
		})
		s.Require().NoError(err)
	}
	propose := func() proposals.Proposal {
		s.T().Helper()
		s.define(t.Owner, "notes.add", mandates.RiskLow, true)
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "notes.add", Title: "note", Payload: json.RawMessage(`{"anything":1}`)})
		s.Require().NoError(err)
		strict()
		return p
	}

	// A person cannot approve it as it stands, and can with an edit that fits.
	p := propose()
	_, err := s.W.Kernel.Proposals.Approve(t.Reviewer, p.Id, proposals.Decision{})
	s.code(err, fault.Conflict)
	s.Require().Contains(err.Error(), "schema has changed")
	still, err := s.W.Kernel.Proposals.Get(t.Owner, p.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusPending, still.Status)
	_, err = s.W.Kernel.Proposals.Approve(t.Reviewer, p.Id, proposals.Decision{Payload: json.RawMessage(`{"anything":2}`)})
	s.code(err, fault.InvalidArgument)
	approved, err := s.W.Kernel.Proposals.Approve(t.Reviewer, p.Id, proposals.Decision{Payload: json.RawMessage(`{"text":"hello"}`)})
	s.Require().NoError(err)
	s.Require().True(approved.Edited)
	s.Require().JSONEq(`{"text":"hello"}`, string(approved.Payload))

	// It can always be rejected.
	rejected := propose()
	_, err = s.W.Kernel.Proposals.Reject(t.Reviewer, rejected.Id, "no longer fits")
	s.Require().NoError(err)

	// A mandate does not allow it either: it is left for a person, with the reason.
	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	waiting := propose()
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, waiting.Id))
	after, err := s.W.Kernel.Proposals.Get(t.Owner, waiting.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusPending, after.Status)
	reason, found, err := s.W.Kernel.Proposals.SkipReason(t.Owner, waiting.Id)
	s.Require().NoError(err)
	s.Require().True(found)
	s.Require().Equal(proposals.SkipPayloadInvalid, reason)

	// A payload that fits the schema of today is allowed as before.
	fits, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", IdempotencyKey: uuid.NewString(), Action: "notes.add", Title: "note", Payload: json.RawMessage(`{"text":"hi"}`)})
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, fits.Id))
	done, err := s.W.Kernel.Proposals.Get(t.Owner, fits.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusExecuted, done.Status)
}

// eventPayloads maps each event type about a proposal to its payload.
func (s *HardeningSuite) eventPayloads(ctx context.Context, id uuid.UUID) map[string]map[string]any {
	s.T().Helper()
	history, err := s.W.Kernel.Proposals.History(ctx, id)
	s.Require().NoError(err)
	out := map[string]map[string]any{}
	for _, e := range history {
		var payload map[string]any
		s.Require().NoError(json.Unmarshal(e.Payload, &payload))
		out[e.Type] = payload
	}
	return out
}

// The chain names the payload that was proposed and the one decided on,
// so a payload rewritten in the table afterwards no longer matches.
func (s *HardeningSuite) Test_TheChainNamesWhatWasDecided() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.define(t.Owner, "notes.add", mandates.RiskLow, true)
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", Action: "notes.add", Title: "note", Payload: json.RawMessage(`{"b":2, "a":1}`), IdempotencyKey: "k1"})
	s.Require().NoError(err)
	proposed, err := events.Digest(json.RawMessage(`{"a":1,"b":2}`))
	s.Require().NoError(err)
	created := s.eventPayloads(t.Owner, p.Id)["proposal.created"]
	s.Require().Equal(proposed, created["payloadSha256"], "the digest is of the canonical form")
	s.Require().Len(created["requestSha256"], 64)

	approved, err := s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{Payload: json.RawMessage(`{"a":1,"b":3}`)})
	s.Require().NoError(err)
	edited, err := events.Digest(json.RawMessage(`{"a":1,"b":3}`))
	s.Require().NoError(err)
	s.Require().Equal(edited, s.eventPayloads(t.Owner, p.Id)["proposal.approved"]["payloadSha256"])
	s.Require().JSONEq(`{"a":1,"b":2}`, string(approved.ProposedPayload), "what the agent proposed is kept beside the edit")
	s.Require().JSONEq(`{"a":1,"b":3}`, string(approved.Payload))

	// The proposed payload is the database's to set, and nobody changes it.
	s.refused(t.Owner, `UPDATE mandate.proposals SET proposed_payload = '{"a":9}' WHERE id = $1`, p.Id)

	// An execution names its payload too.
	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"*"}, MaxRisk: mandates.RiskLow, BudgetCount: 10, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	q, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", Action: "notes.add", Title: "note", Payload: json.RawMessage(`{"c":3}`), IdempotencyKey: "k2"})
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, q.Id))
	third, err := events.Digest(json.RawMessage(`{"c":3}`))
	s.Require().NoError(err)
	s.Require().Equal(third, s.eventPayloads(t.Owner, q.Id)["proposal.executed"]["payloadSha256"])
}

// A change to the action after the agent proposed is not carried along:
// a person approves at the risk the action has now, and a mandate leaves
// it to a person.
func (s *HardeningSuite) Test_TheActionAsItIsNowDecides() {
	t := s.W.NewTenant(s.T(), "Acme")
	s.define(t.Owner, "notes.add", mandates.RiskLow, true)
	m, err := s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{Actions: []string{"*"}, MaxRisk: mandates.RiskMedium, BudgetCount: 10, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	propose := func(key string) proposals.Proposal {
		s.T().Helper()
		p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", Action: "notes.add", Title: "note", Payload: json.RawMessage(`{}`), IdempotencyKey: key})
		s.Require().NoError(err)
		return p
	}
	skipped := func(p proposals.Proposal) string {
		s.T().Helper()
		reason, _, err := s.W.Kernel.Proposals.SkipReason(t.Owner, p.Id)
		s.Require().NoError(err)
		return reason
	}

	// The risk went up after the proposal: not executed, for a person.
	low := propose("low")
	s.define(t.Owner, "notes.add", mandates.RiskMedium, true)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, low.Id))
	s.Require().Equal(proposals.SkipActionChanged, skipped(low))
	basis := map[string]any{"mandateId": m.Id.String(), "policyRef": "P", "maxRisk": "medium", "budgets": []any{map[string]any{"kind": "count", "limit": 10, "spent": 0}}}
	s.code(fault.FromDb(s.executeDirectly(t.System, low.Id, basis)), fault.PermissionDenied)

	// A person approves it at the risk the action has now.
	s.define(t.Owner, "notes.add", mandates.RiskHigh, false)
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, low.Id, proposals.Decision{})
	s.code(err, fault.InvalidArgument)
	s.Require().Contains(err.Error(), "confirmed", "now high, so it must be confirmed")
	approved, err := s.W.Kernel.Proposals.Approve(t.Owner, low.Id, proposals.Decision{Confirm: true})
	s.Require().NoError(err)
	s.Require().Equal(mandates.RiskHigh, approved.Risk)

	// A proposal made under the action as it is now runs as before.
	s.define(t.Owner, "notes.add", mandates.RiskLow, true)
	fits := propose("fits")
	s.Require().NoError(s.W.Kernel.ExecuteProposal(context.Background(), t.Id, fits.Id))
	done, err := s.W.Kernel.Proposals.Get(t.Owner, fits.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusExecuted, done.Status)
}

// A delivery exists only for a decision made in the same transaction, so
// an old decision is never sent to a connector defined after it.
func (s *HardeningSuite) Test_ADeliveryBelongsToItsDecision() {
	t := s.W.NewTenant(s.T(), "Acme")
	// test.low is carried out by code, so its approval has no delivery.
	p, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Test rationale.", Action: "test.low", Title: "note", Payload: json.RawMessage(`{}`), IdempotencyKey: "k1"})
	s.Require().NoError(err)
	_, err = s.W.Kernel.Proposals.Approve(t.Owner, p.Id, proposals.Decision{})
	s.Require().NoError(err)
	// Later the tenant defines it as a webhook, and someone records a
	// delivery for the old approval.
	s.define(t.Owner, "test.low", mandates.RiskLow, false)
	s.refused(t.Owner, `INSERT INTO mandate.deliveries (id, tenant_id, proposal_id, action_key, url) VALUES (gen_random_uuid(), $1, $2, 'test.low', 'https://connector.example/hooks')`, t.Id, p.Id)
}
