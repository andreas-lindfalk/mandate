// Package api serves people over Connect: sign-in, the inbox, the
// mandates, the agents and actions, the people, the notifier and the
// evidence (the log and its exports). Every call carries a
// session and, except sign-in, the tenant it acts in.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/andreas-lindfalk/pgmandate"
	mandatev1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1/mandatev1connect"
	"github.com/andreas-lindfalk/pgmandate/modules/auth"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// maxRequestBytes bounds a request body.
const maxRequestBytes = 1 << 20

type Handler struct {
	kernel *pgmandate.Kernel
	auth   *auth.Service
}

func New(kernel *pgmandate.Kernel, authService *auth.Service) *Handler {
	return &Handler{kernel: kernel, auth: authService}
}

// Mount adds every service to the mux, behind the session interceptor.
func (h *Handler) Mount(mux *http.ServeMux) {
	public := map[string]bool{mandatev1connect.AuthServiceDevLoginProcedure: true, mandatev1connect.AuthServiceSignInMethodsProcedure: true}
	tenantFree := map[string]bool{mandatev1connect.AuthServiceLogoutProcedure: true, mandatev1connect.AuthServiceMeProcedure: true}
	options := []connect.HandlerOption{
		connect.WithInterceptors(auth.NewInterceptor(h.auth, public, tenantFree)),
		connect.WithReadMaxBytes(maxRequestBytes),
	}
	mux.Handle(mandatev1connect.NewAuthServiceHandler(&authHandler{h}, options...))
	mux.Handle(mandatev1connect.NewPersonServiceHandler(&personHandler{h}, options...))
	mux.Handle(mandatev1connect.NewAgentServiceHandler(&agentHandler{h}, options...))
	mux.Handle(mandatev1connect.NewActionServiceHandler(&actionHandler{h}, options...))
	mux.Handle(mandatev1connect.NewMandateServiceHandler(&mandateHandler{h}, options...))
	mux.Handle(mandatev1connect.NewProposalServiceHandler(&proposalHandler{h}, options...))
	mux.Handle(mandatev1connect.NewEvidenceServiceHandler(&evidenceHandler{h}, options...))
	mux.Handle(mandatev1connect.NewNotifierServiceHandler(&notifierHandler{h}, options...))
	h.auth.Mount(mux)
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tsPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func tsText(text *string) *timestamppb.Timestamp {
	if text == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *text)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}

func idText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func parseId(value, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return uuid.Nil, fault.ToConnect(fault.New(fault.InvalidArgument, "invalid "+what))
	}
	return id, nil
}

func jsonText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

func (h *Handler) secure() bool {
	return strings.HasPrefix(h.auth.Config().PublicUrl, "https://")
}

// --- auth

type authHandler struct{ *Handler }

var _ mandatev1connect.AuthServiceHandler = (*authHandler)(nil)

func (h *authHandler) SignInMethods(context.Context, *mandatev1.SignInMethodsRequest) (*mandatev1.SignInMethodsResponse, error) {
	return &mandatev1.SignInMethodsResponse{DevLogin: h.auth.Config().DevLogin, Oidc: h.auth.HasOidc()}, nil
}

func (h *authHandler) DevLogin(ctx context.Context, req *mandatev1.DevLoginRequest) (*mandatev1.DevLoginResponse, error) {
	token, err := h.auth.DevLogin(ctx, req.GetEmail(), req.GetName())
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	if info, ok := connect.CallInfoForHandlerContext(ctx); ok {
		cookie := auth.SessionCookie(token, h.auth.Config().SessionTtl, h.secure())
		info.ResponseHeader().Add("Set-Cookie", cookie.String())
	}
	return &mandatev1.DevLoginResponse{Token: token}, nil
}

func (h *authHandler) Logout(ctx context.Context, _ *mandatev1.LogoutRequest) (*mandatev1.LogoutResponse, error) {
	if err := h.auth.Logout(ctx, auth.TokenFrom(ctx)); err != nil {
		return nil, fault.ToConnect(err)
	}
	if info, ok := connect.CallInfoForHandlerContext(ctx); ok {
		info.ResponseHeader().Add("Set-Cookie", auth.ClearedSessionCookie(h.secure()).String())
	}
	return &mandatev1.LogoutResponse{}, nil
}

func (h *authHandler) Me(ctx context.Context, _ *mandatev1.MeRequest) (*mandatev1.MeResponse, error) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return nil, fault.ToConnect(fault.New(fault.Unauthenticated, "missing session"))
	}
	memberships, err := h.auth.Memberships(ctx, session)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	resp := &mandatev1.MeResponse{Email: session.Email, Name: session.Name, DevLogin: h.auth.Config().DevLogin, Oidc: h.auth.HasOidc()}
	for _, m := range memberships {
		resp.Tenants = append(resp.Tenants, &mandatev1.TenantMembership{TenantId: m.TenantId.String(), TenantName: m.TenantName, PersonId: m.PersonId.String(), Role: m.Role})
	}
	return resp, nil
}

// --- persons

type personHandler struct{ *Handler }

var _ mandatev1connect.PersonServiceHandler = (*personHandler)(nil)

func (h *personHandler) ListPersons(ctx context.Context, _ *mandatev1.ListPersonsRequest) (*mandatev1.ListPersonsResponse, error) {
	list, err := h.kernel.Tenants.ListPersons(ctx)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	resp := &mandatev1.ListPersonsResponse{}
	for _, p := range list {
		resp.Persons = append(resp.Persons, &mandatev1.Person{Id: p.Id.String(), Email: p.Email, Name: p.Name, Role: p.Role, CreatedAt: ts(p.CreatedAt), RemovedAt: tsPtr(p.RemovedAt)})
	}
	return resp, nil
}

func (h *personHandler) RemovePerson(ctx context.Context, req *mandatev1.RemovePersonRequest) (*mandatev1.RemovePersonResponse, error) {
	id, err := parseId(req.GetId(), "person id")
	if err != nil {
		return nil, err
	}
	if err := h.kernel.Tenants.RemovePerson(ctx, id); err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.RemovePersonResponse{}, nil
}

func (h *personHandler) AddPerson(ctx context.Context, req *mandatev1.AddPersonRequest) (*mandatev1.AddPersonResponse, error) {
	p, err := h.kernel.Tenants.AddPerson(ctx, req.GetEmail(), req.GetName(), req.GetRole())
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.AddPersonResponse{Person: &mandatev1.Person{Id: p.Id.String(), Email: p.Email, Name: p.Name, Role: p.Role, CreatedAt: ts(p.CreatedAt)}}, nil
}
