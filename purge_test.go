package pgmandate_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/examples/webhook"
	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/auth"
	"github.com/andreas-lindfalk/pgmandate/modules/deliveries"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/notifications"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
)

// PurgeSuite tests purge_tenant, the one way a tenant's rows, append-only
// ones included, leave the database.
type PurgeSuite struct {
	IntegrationSuite
}

func TestPurgeSuite(t *testing.T) {
	suite.Run(t, new(PurgeSuite))
}

// tenantTables lists the tables of pgmandate's schema that hold rows of a
// tenant. A table added later is found by itself, and the test below then
// asks for a row of the purged tenant in it.
func (s *PurgeSuite) tenantTables() []string {
	s.T().Helper()
	rows, err := s.W.Pg.Owner.Query(context.Background(), `
		SELECT c.table_name FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = 'mandate' AND c.column_name = 'tenant_id' AND t.table_type = 'BASE TABLE'
		ORDER BY c.table_name`)
	s.Require().NoError(err)
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	s.Require().NoError(err)
	s.Require().NotEmpty(tables)
	return tables
}

// rows counts a tenant's rows in each of tables, its tenant row, and the
// sessions pinned to its people, as the owner, past row level security.
func (s *PurgeSuite) rows(tenantId uuid.UUID, tables []string, people []uuid.UUID) map[string]int {
	s.T().Helper()
	ctx := context.Background()
	counts := map[string]int{}
	for _, table := range tables {
		var n int
		s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx,
			`SELECT count(*) FROM `+pgx.Identifier{"mandate", table}.Sanitize()+` WHERE tenant_id = $1`, tenantId).Scan(&n), table)
		counts[table] = n
	}
	var n int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx, `SELECT count(*) FROM mandate.tenants WHERE id = $1`, tenantId).Scan(&n))
	counts["tenants"] = n
	s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx, `SELECT count(*) FROM mandate.sessions WHERE person_id = ANY($1)`, people).Scan(&n))
	counts["sessions"] = n
	return counts
}

// verified checks a tenant's chain in its own scope and returns its length.
func (s *PurgeSuite) verified(t testfix.Tenant) int64 {
	s.T().Helper()
	var count int64
	s.Require().NoError(db.InTx(t.Owner, s.W.Pg.App, func(tx pgx.Tx) error {
		var err error
		count, err = events.Verify(t.Owner, tx)
		return err
	}))
	return count
}

// A tenant with a row in every table that holds tenant data, append-only
// and guarded ones included, is purged entirely, and another tenant's rows
// and chain stay as they were. Every test tenant is purged when its test
// ends, but most hold little; this one holds a delivery that has been
// attempted, whose guards refuse every other delete.
func (s *PurgeSuite) Test_APurgeRemovesEveryRowOfATenantAndNoOtherTenants() {
	ctx := context.Background()
	t := s.W.NewTenant(s.T(), "Acme")
	other := s.W.NewTenant(s.T(), "Other")
	otherWaiting, _, err := s.W.Kernel.Proposals.Propose(other.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "test.manual", Title: "Other", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)

	// A webhook action, a mandate that allows it, and a proposal the
	// mandate executes, with its delivery attempted and answered.
	receiver := webhook.NewReceiver(testfix.Keys)
	connector := httptest.NewServer(receiver)
	defer connector.Close()
	defined, err := s.W.Kernel.Actions.Define(t.Owner, actions.Input{
		Key: "tickets.create", Risk: mandates.RiskLow, MayExecute: true, Connector: actions.ConnectorWebhook, Url: connector.URL,
		Schema: json.RawMessage(`{"type":"object"}`),
	})
	s.Require().NoError(err)
	receiver.Trust(defined.ConnectorAudience, t.Id)
	_, err = s.W.Kernel.Mandates.Grant(t.Owner, mandates.Input{AgentId: &t.AgentId, Actions: []string{"tickets.*"}, MaxRisk: mandates.RiskLow, BudgetCount: 5, PolicyRef: "P", Days: 30})
	s.Require().NoError(err)
	executed, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "tickets.create", Title: "Ticket", Payload: json.RawMessage(`{"subject":"x"}`)})
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(ctx, t.Id, executed.Id))
	delivery, found, err := s.W.Kernel.Deliveries.Get(t.Owner, executed.Id)
	s.Require().NoError(err)
	s.Require().True(found)
	s.Require().NoError(s.W.Kernel.Deliveries.Deliver(ctx, t.Id, delivery.Id, false))
	delivery, _, err = s.W.Kernel.Deliveries.Get(t.Owner, executed.Id)
	s.Require().NoError(err)
	s.Require().Equal(deliveries.StatusDone, delivery.Status)
	s.Require().Equal(1, delivery.AttemptsStarted)
	s.Require().Len(receiver.Received(), 1)

	// A proposal the mandate does not cover, skipped and left for a person.
	skipped, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{Rationale: "Why.", IdempotencyKey: uuid.NewString(), Action: "test.manual", Title: "Manual", Payload: json.RawMessage(`{}`)})
	s.Require().NoError(err)
	s.Require().NoError(s.W.Kernel.ExecuteProposal(ctx, t.Id, skipped.Id))
	_, found, err = s.W.Kernel.Proposals.SkipReason(t.Owner, skipped.Id)
	s.Require().NoError(err)
	s.Require().True(found, "the execution was skipped")

	// A notifier, a person's link, and the session it gave, pinned to them.
	_, err = s.W.Kernel.Notifications.Set(t.Owner, notifications.KindSlack, "https://hooks.slack.com/services/T000/B000/purge")
	s.Require().NoError(err)
	signIn := auth.NewService(s.W.Pg.App, auth.Config{SessionTtl: time.Hour, PublicUrl: "http://127.0.0.1"})
	link, err := signIn.NewPersonLink(t.System, t.ReviewerId)
	s.Require().NoError(err)
	_, err = signIn.RedeemLink(ctx, link)
	s.Require().NoError(err)
	_, err = signIn.NewPersonLink(t.System, t.OwnerId)
	s.Require().NoError(err, "and one not used")

	tables := s.tenantTables()
	people := []uuid.UUID{t.OwnerId, t.ReviewerId}
	before := s.rows(t.Id, tables, people)
	for table, n := range before {
		s.Require().NotZero(n, "the tenant has rows in %s before the purge", table)
	}
	otherPeople := []uuid.UUID{other.OwnerId, other.ReviewerId}
	otherBefore := s.rows(other.Id, tables, otherPeople)
	otherChain := s.verified(other)

	s.Require().NoError(s.W.Pg.PurgeTenant(ctx, t.Id))

	for table, n := range s.rows(t.Id, tables, people) {
		s.Require().Zero(n, "the purge leaves nothing of the tenant in %s", table)
	}
	s.Require().Equal(otherBefore, s.rows(other.Id, tables, otherPeople), "the other tenant keeps every row")
	s.Require().Equal(otherChain, s.verified(other), "and its chain verifies as before")
	still, err := s.W.Kernel.Proposals.Get(other.Owner, otherWaiting.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusPending, still.Status)
}
