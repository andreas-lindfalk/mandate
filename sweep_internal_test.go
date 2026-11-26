package pgmandate

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
)

// A tenant whose sweep fails does not keep the tenants after it from being
// swept: every tenant is visited as the system scoped to it, the counts add
// up, and every failure is returned, naming its tenant.
func TestForEachTenantVisitsEveryTenant(t *testing.T) {
	first, second, third := uuid.New(), uuid.New(), uuid.New()
	failure := errors.New("refused")
	var visited []uuid.UUID
	total, err := forEachTenant(context.Background(), []uuid.UUID{first, second, third}, func(ctx context.Context) (int, error) {
		a, ok := actor.From(ctx)
		require.True(t, ok)
		require.Equal(t, actor.System, a.Kind)
		tenantId, ok := actor.TenantFrom(ctx)
		require.True(t, ok)
		visited = append(visited, tenantId)
		switch tenantId {
		case first:
			return 1, failure
		case second:
			return 2, nil
		default:
			return 3, failure
		}
	})
	require.Equal(t, []uuid.UUID{first, second, third}, visited, "a failing tenant does not stop the sweep")
	require.Equal(t, 6, total, "what was done before and after a failure is counted")
	require.ErrorIs(t, err, failure)
	require.ErrorContains(t, err, "tenant "+first.String()+": refused")
	require.ErrorContains(t, err, "tenant "+third.String()+": refused")
	require.NotContains(t, err.Error(), second.String(), "a tenant that succeeded is not reported")

	total, err = forEachTenant(context.Background(), []uuid.UUID{second}, func(context.Context) (int, error) { return 2, nil })
	require.NoError(t, err, "no failure, no error")
	require.Equal(t, 2, total)
}
