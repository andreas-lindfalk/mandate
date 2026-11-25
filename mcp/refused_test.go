package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate/modules/deliveries"
)

// Only a final 4xx on the receiver's own rules is a refusal; statuses
// retried until the attempts ran out say nothing about the request.
func TestRefused(t *testing.T) {
	for status, want := range map[int]bool{400: true, 403: true, 404: true, 409: true, 422: true, 401: false, 408: false, 429: false, 500: false, 503: false} {
		require.Equal(t, want, refused(&deliveries.Delivery{LastStatus: &status}), status)
	}
	require.False(t, refused(&deliveries.Delivery{}), "no answer at all")
}
