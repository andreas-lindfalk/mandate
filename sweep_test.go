package pgmandate_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
)

// SweepSuite tests the periodic sweep through River, and where River lives.
type SweepSuite struct {
	IntegrationSuite
}

func TestSweepSuite(t *testing.T) {
	suite.Run(t, &SweepSuite{IntegrationSuite: IntegrationSuite{WithJobs: true}})
}

func (s *SweepSuite) Test_TheSweepExpiresWhatIsDue() {
	t := s.W.NewTenant(s.T(), "Acme")
	ctx := context.Background()
	due, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{IdempotencyKey: uuid.NewString(), Action: "test.manual", Title: "x", Payload: json.RawMessage(`{}`), Rationale: "Why."})
	s.Require().NoError(err)
	waiting, _, err := s.W.Kernel.Proposals.Propose(t.Agent, proposals.Input{IdempotencyKey: uuid.NewString(), Action: "test.manual", Title: "y", Payload: json.RawMessage(`{}`), Rationale: "Why."})
	s.Require().NoError(err)
	_, err = s.W.Pg.Owner.Exec(ctx, `
		BEGIN;
		SET LOCAL session_replication_role = replica;
		UPDATE mandate.proposals SET expires_at = now() - interval '1 second' WHERE id = '`+due.Id.String()+`';
		COMMIT;`)
	s.Require().NoError(err)

	// What River's periodic job inserts every minute, inserted now.
	s.Require().NoError(s.W.Queue.Insert(ctx, pgmandate.SweepArgs{}, nil))
	s.Require().Eventually(func() bool {
		p, err := s.W.Kernel.Proposals.Get(t.Owner, due.Id)
		return err == nil && p.Status == proposals.StatusExpired
	}, 15*time.Second, 100*time.Millisecond, "the sweep expires the proposal past its deadline")
	still, err := s.W.Kernel.Proposals.Get(t.Owner, waiting.Id)
	s.Require().NoError(err)
	s.Require().Equal(proposals.StatusPending, still.Status, "and leaves the one still in time")
}

// River's tables live in a schema of pgmandate's own, and the application
// role is given nothing on River tables in public, where a host's own River
// would live. (The helpdesk example's own tables in public are granted by
// that host, as an embedded host does.)
func (s *SweepSuite) Test_RiverLivesInItsOwnSchema() {
	ctx := context.Background()
	var own, public int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE schemaname = $1), count(*) FILTER (WHERE schemaname = 'public')
		 FROM pg_tables WHERE tablename LIKE 'river\_%'`, jobs.Schema).Scan(&own, &public))
	s.Require().Positive(own)
	s.Require().Zero(public)
	var inOwn, riverInPublic int
	s.Require().NoError(s.W.Pg.Owner.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE table_schema = $1), count(*) FILTER (WHERE table_schema = 'public' AND table_name LIKE 'river\_%')
		 FROM information_schema.role_table_grants WHERE grantee = 'mandate_app'`, jobs.Schema).Scan(&inOwn, &riverInPublic))
	s.Require().Positive(inOwn, "the app role works River's tables in their own schema")
	s.Require().Zero(riverInPublic, "and is given nothing on River tables in public, where a host's own River would live")
}
