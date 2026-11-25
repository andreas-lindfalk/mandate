package fault

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// Only pgmandate's own code is a rate limit; Postgres's own limits stay
// internal, with their message kept from the caller.
func TestFromDbMapsOnlyOurLimit(t *testing.T) {
	ours := FromDb(&pgconn.PgError{Code: "MD429", Message: "this agent may make 30 proposals a minute"})
	require.Equal(t, RateLimited, CodeOf(ours))
	require.Equal(t, "this agent may make 30 proposals a minute", Message(ours))
	for _, code := range []string{"54000", "54001"} {
		theirs := FromDb(&pgconn.PgError{Code: code, Message: "index row size exceeds maximum"})
		require.Equal(t, Internal, CodeOf(theirs), code)
		require.Equal(t, "internal error", Message(theirs))
	}
	require.Equal(t, "rate_limited", Name(RateLimited))
}
