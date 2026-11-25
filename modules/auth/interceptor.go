package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// TenantHeader selects the tenant a call acts in.
const TenantHeader = "Mandate-Tenant"

type sessionKey struct{}

// SessionFrom returns the signed-in person of a call.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(Session)
	return s, ok
}

type tokenKey struct{}

// TokenFrom returns the session token of a call, for logout.
func TokenFrom(ctx context.Context) string {
	token, _ := ctx.Value(tokenKey{}).(string)
	return token
}

// NewInterceptor authenticates the session, sets the actor and, when the
// procedure is tenant scoped, checks that the person belongs to the tenant
// in the Mandate-Tenant header before the handler runs. Procedures in
// public need no session; those in tenantFree need no tenant.
func NewInterceptor(service *Service, public, tenantFree map[string]bool) connect.Interceptor {
	return &interceptor{service: service, public: public, tenantFree: tenantFree}
}

type interceptor struct {
	service    *Service
	public     map[string]bool
	tenantFree map[string]bool
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, err := i.authenticate(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(context.Context, connect.StreamingHandlerConn) error {
		return connect.NewError(connect.CodeUnimplemented, errors.New("streaming calls are not supported"))
	}
}

func (i *interceptor) authenticate(ctx context.Context, procedure string, header http.Header) (context.Context, error) {
	if i.public[procedure] {
		return ctx, nil
	}
	token, ok := TokenOf(header)
	if !ok {
		return nil, fault.ToConnect(fault.New(fault.Unauthenticated, "missing session"))
	}
	session, err := i.service.Authenticate(ctx, token)
	if err != nil {
		refused := fault.ToConnect(err)
		// A session cookie that no longer opens anything is cleared, so a
		// page left open does not keep sending it, and keep failing.
		var connectErr *connect.Error
		if fault.CodeOf(err) == fault.Unauthenticated && !bearer(header) && errors.As(refused, &connectErr) {
			connectErr.Meta().Add("Set-Cookie", ClearedSessionCookie(i.service.secure()).String())
		}
		return nil, refused
	}
	ctx = context.WithValue(context.WithValue(ctx, sessionKey{}, session), tokenKey{}, token)
	if i.tenantFree[procedure] {
		return ctx, nil
	}
	tenantId, err := uuid.Parse(header.Get(TenantHeader))
	if err != nil {
		return nil, fault.ToConnect(fault.New(fault.InvalidArgument, "missing or invalid "+TenantHeader+" header"))
	}
	membership, err := i.service.Membership(ctx, session, tenantId)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return actor.AsPerson(ctx, tenantId, membership.PersonId), nil
}

// bearer says the token came in the Authorization header, not the cookie.
func bearer(header http.Header) bool {
	token, ok := strings.CutPrefix(header.Get("Authorization"), "Bearer ")
	return ok && strings.TrimSpace(token) != ""
}

// TokenOf reads the session token from the Authorization header, or from
// the session cookie.
func TokenOf(header http.Header) (string, bool) {
	if token, ok := strings.CutPrefix(header.Get("Authorization"), "Bearer "); ok && strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token), true
	}
	request := http.Request{Header: header}
	if cookie, err := request.Cookie(CookieName); err == nil && cookie.Value != "" {
		return cookie.Value, true
	}
	return "", false
}
