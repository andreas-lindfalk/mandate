package api

import (
	"context"

	mandatev1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1/mandatev1connect"
	"github.com/andreas-lindfalk/pgmandate/modules/agents"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type agentHandler struct{ *Handler }

var _ mandatev1connect.AgentServiceHandler = (*agentHandler)(nil)

func toAgent(a agents.Agent) *mandatev1.Agent {
	return &mandatev1.Agent{Id: a.Id.String(), Name: a.Name, Department: a.Department, CreatedAt: ts(a.CreatedAt), RevokedAt: tsPtr(a.RevokedAt)}
}

func toToken(t agents.Token) *mandatev1.Token {
	return &mandatev1.Token{
		Id: t.Id.String(), AgentId: t.AgentId.String(), Hint: t.Hint, CreatedAt: ts(t.CreatedAt), ExpiresAt: ts(t.ExpiresAt),
		LastUsedAt: tsPtr(t.LastUsedAt), RevokedAt: tsPtr(t.RevokedAt),
	}
}

func (h *agentHandler) ListAgents(ctx context.Context, _ *mandatev1.ListAgentsRequest) (*mandatev1.ListAgentsResponse, error) {
	list, err := h.kernel.Agents.List(ctx)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	resp := &mandatev1.ListAgentsResponse{}
	for _, a := range list {
		resp.Agents = append(resp.Agents, toAgent(a))
	}
	return resp, nil
}

func (h *agentHandler) CreateAgent(ctx context.Context, req *mandatev1.CreateAgentRequest) (*mandatev1.CreateAgentResponse, error) {
	a, err := h.kernel.Agents.Create(ctx, req.GetName(), req.GetDepartment())
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.CreateAgentResponse{Agent: toAgent(a)}, nil
}

func (h *agentHandler) RevokeAgent(ctx context.Context, req *mandatev1.RevokeAgentRequest) (*mandatev1.RevokeAgentResponse, error) {
	id, err := parseId(req.GetId(), "agent id")
	if err != nil {
		return nil, err
	}
	if err := h.kernel.Agents.Revoke(ctx, id); err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.RevokeAgentResponse{}, nil
}

func (h *agentHandler) IssueToken(ctx context.Context, req *mandatev1.IssueTokenRequest) (*mandatev1.IssueTokenResponse, error) {
	id, err := parseId(req.GetAgentId(), "agent id")
	if err != nil {
		return nil, err
	}
	token, secret, err := h.kernel.Agents.IssueToken(ctx, id, int(req.GetDays()))
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.IssueTokenResponse{Token: toToken(token), Secret: secret}, nil
}

func (h *agentHandler) ListTokens(ctx context.Context, req *mandatev1.ListTokensRequest) (*mandatev1.ListTokensResponse, error) {
	id, err := parseId(req.GetAgentId(), "agent id")
	if err != nil {
		return nil, err
	}
	list, err := h.kernel.Agents.ListTokens(ctx, id)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	resp := &mandatev1.ListTokensResponse{}
	for _, t := range list {
		resp.Tokens = append(resp.Tokens, toToken(t))
	}
	return resp, nil
}

func (h *agentHandler) RevokeToken(ctx context.Context, req *mandatev1.RevokeTokenRequest) (*mandatev1.RevokeTokenResponse, error) {
	id, err := parseId(req.GetId(), "token id")
	if err != nil {
		return nil, err
	}
	if err := h.kernel.Agents.RevokeToken(ctx, id); err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.RevokeTokenResponse{}, nil
}
