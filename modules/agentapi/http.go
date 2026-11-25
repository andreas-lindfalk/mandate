package agentapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/agent/v1/agentv1connect"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// maxRequestBytes bounds a request body.
const maxRequestBytes = 1 << 20

// Handler serves the agent contract as Connect, JSON or protobuf over
// HTTP, at the path it returns. A call carries the agent's token as
// "Authorization: Bearer mnd_..."; nothing else authenticates it, so a
// person's session, as a cookie or a header, is refused.
func (s *Service) Handler() (string, http.Handler) {
	return agentv1connect.NewAgentServiceHandler(s,
		connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				// The scheme is case-insensitive, as over MCP.
				scheme, token, ok := strings.Cut(strings.TrimSpace(req.Header().Get("Authorization")), " ")
				if !ok || !strings.EqualFold(scheme, "Bearer") {
					return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("an agent token is required, as Authorization: Bearer mnd_<token>"))
				}
				grant, err := s.Authenticate(ctx, strings.TrimSpace(token))
				if err != nil {
					if fault.CodeOf(err) == fault.Unauthenticated {
						return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid agent token"))
					}
					slog.ErrorContext(ctx, "authenticate agent token", "error", err)
					return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
				}
				return next(WithCall(ctx, Call{Grant: grant, Via: ViaHttp}), req)
			}
		})),
		connect.WithReadMaxBytes(maxRequestBytes),
	)
}
