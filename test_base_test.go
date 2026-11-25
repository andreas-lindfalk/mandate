package pgmandate_test

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
)

// IntegrationSuite starts a real Postgres and a kernel, with a job queue
// when WithJobs is set. Each test file embeds it in its own suite.
type IntegrationSuite struct {
	suite.Suite
	WithJobs bool
	W        *testfix.World
}

func (s *IntegrationSuite) SetupSuite() {
	w, err := testfix.Start(context.Background(), s.WithJobs)
	s.Require().NoError(err)
	s.W = w
}

func (s *IntegrationSuite) TearDownSuite() {
	s.W.Close()
}

func (s *IntegrationSuite) eventTypes(ctx context.Context, subject uuid.UUID) []string {
	s.T().Helper()
	var list []events.Stored
	s.Require().NoError(db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		var err error
		list, err = events.List(ctx, tx, &subject, 0)
		return err
	}))
	types := make([]string, 0, len(list))
	for _, e := range list {
		types = append(types, e.Type)
	}
	return types
}
