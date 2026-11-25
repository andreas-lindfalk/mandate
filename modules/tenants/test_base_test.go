package tenants_test

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// IntegrationSuite starts a real Postgres with the schema and a kernel
// without a job queue. Each test file embeds it in its own suite.
type IntegrationSuite struct {
	suite.Suite
	W *testfix.World
}

func (s *IntegrationSuite) SetupSuite() {
	w, err := testfix.Start(context.Background(), false)
	s.Require().NoError(err)
	s.W = w
}

func (s *IntegrationSuite) TearDownSuite() {
	s.W.Close()
}

// code asserts an error's code.
func (s *IntegrationSuite) code(err error, want fault.Code) {
	s.T().Helper()
	s.Require().Error(err)
	s.Require().Equal(fault.Name(want), fault.Name(fault.CodeOf(err)), err.Error())
}

// eventTypes lists the types of the events about a subject, in order.
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

// exec runs one statement as the app role in the scope of ctx.
func (s *IntegrationSuite) exec(ctx context.Context, statement string, args ...any) error {
	return db.InTx(ctx, s.W.Pg.App, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, statement, args...)
		return err
	})
}

// refused asserts that the database refused a statement with a trigger's
// or a policy's insufficient privilege (SQLSTATE 42501).
func (s *IntegrationSuite) refused(ctx context.Context, statement string, args ...any) {
	s.T().Helper()
	err := s.exec(ctx, statement, args...)
	s.Require().Error(err, statement)
	s.Require().Equal(fault.Name(fault.PermissionDenied), fault.Name(fault.CodeOf(fault.FromDb(err))), err.Error())
}
