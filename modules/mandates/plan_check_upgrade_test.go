package mandates_test

import (
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/schema"
)

// Migration 0004 made the database's check of a plan stricter. A check is
// held on every update of a row, a revocation included, so a row that was
// written under the old check and fails the new one must still be
// revocable. This suite has a database of its own, since it puts the old
// check back for a moment.
type PlanCheckUpgradeSuite struct {
	IntegrationSuite
}

func TestPlanCheckUpgradeSuite(t *testing.T) {
	suite.Run(t, new(PlanCheckUpgradeSuite))
}

func (s *PlanCheckUpgradeSuite) migration(name string) {
	s.T().Helper()
	sql, err := fs.ReadFile(schema.Migrations.Fs, name)
	s.Require().NoError(err)
	_, err = s.W.Pg.Owner.Exec(s.T().Context(), string(sql))
	s.Require().NoError(err, name)
}

func (s *PlanCheckUpgradeSuite) Test_AMandateWrittenUnderTheOldCheckCanStillBeRevoked() {
	t := s.W.NewTenant(s.T(), "Acme")
	insert := `INSERT INTO mandate.mandates (id, tenant_id, actions, max_risk, budget_count, policy_ref, valid_until, condition, condition_plan)
		VALUES ($1, $2, '{test.*}', 'low', 10, 'P', now() + interval '1 day', 'payload.n == 1', $3::jsonb)`
	// Plans the old check let through and the new one refuses: a field of
	// another operator, and a field missing, for which the old check
	// answered NULL.
	old := map[string]uuid.UUID{
		`{"op":"==","path":["payload","n"],"value":1,"args":[]}`: uuid.New(),
		`{"op":"=="}`:                        uuid.New(),
		`{"op":"and","args":[{"op":"has"}]}`: uuid.New(),
	}

	s.migration("0004_condition_plan_check.down.sql")
	defer s.migration("0004_condition_plan_check.up.sql")
	for plan, id := range old {
		s.Require().NoError(s.exec(t.Owner, insert, id, t.Id, plan), "the old check takes %s", plan)
	}
	s.migration("0004_condition_plan_check.up.sql")

	for plan, id := range old {
		// Written again, it is refused.
		s.invalid(t.Owner, insert, uuid.New(), t.Id, plan)
		// The row that is there can be read and revoked, and stays revoked.
		m, err := s.W.Kernel.Mandates.Get(t.Owner, id)
		s.Require().NoError(err, plan)
		s.Require().False(m.ConditionHolds("test.x", json.RawMessage(`{}`)), "a plan with a field missing, or on a payload without the field, allows nothing: %s", plan)
		s.Require().NoError(s.W.Kernel.Mandates.Revoke(t.Owner, id), "revoking a mandate with the plan %s", plan)
		var revoked bool
		s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
			return tx.QueryRow(t.Owner, `SELECT revoked_at IS NOT NULL FROM mandate.mandates WHERE id = $1`, id).Scan(&revoked)
		}))
		s.Require().True(revoked, plan)
	}

	// A plan the compiler writes is taken as before, live or revoked.
	s.Require().NoError(s.exec(t.Owner, insert, uuid.New(), t.Id, `{"op":"==","path":["payload","n"],"value":1}`))
}
