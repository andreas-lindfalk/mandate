package api

import (
	"context"

	"github.com/google/uuid"

	mandatev1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1/mandatev1connect"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type mandateHandler struct{ *Handler }

var _ mandatev1connect.MandateServiceHandler = (*mandateHandler)(nil)

func toMandate(m mandates.Mandate, spent int64) *mandatev1.Mandate {
	return &mandatev1.Mandate{
		Id: m.Id.String(), AgentId: idText(m.AgentId), Actions: m.Actions, MaxRisk: m.MaxRisk, BudgetUnits: m.BudgetUnits, SpentUnits: spent,
		PolicyRef: m.PolicyRef, Department: m.Department, GrantedBy: idText(m.GrantedBy), ValidFrom: ts(m.ValidFrom), ValidUntil: ts(m.ValidUntil),
		RevokedAt: tsPtr(m.RevokedAt), RevokedBy: idText(m.RevokedBy), Condition: m.Condition,
	}
}

func (h *mandateHandler) ListMandates(ctx context.Context, req *mandatev1.ListMandatesRequest) (*mandatev1.ListMandatesResponse, error) {
	list, err := h.kernel.Mandates.List(ctx, req.GetIncludeEnded())
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, m := range list {
		ids = append(ids, m.Id)
	}
	spent, err := h.kernel.Mandates.SpentFor(ctx, ids)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	resp := &mandatev1.ListMandatesResponse{}
	for _, m := range list {
		resp.Mandates = append(resp.Mandates, toMandate(m, spent[m.Id]))
	}
	return resp, nil
}

func (h *mandateHandler) GrantMandate(ctx context.Context, req *mandatev1.GrantMandateRequest) (*mandatev1.GrantMandateResponse, error) {
	input := mandates.Input{
		Actions: req.GetActions(), MaxRisk: req.GetMaxRisk(), BudgetUnits: req.GetBudgetUnits(), PolicyRef: req.GetPolicyRef(),
		Days: int(req.GetDays()), Department: req.GetDepartment(), Condition: req.GetCondition(),
	}
	if req.GetAgentId() != "" {
		id, err := parseId(req.GetAgentId(), "agent id")
		if err != nil {
			return nil, err
		}
		input.AgentId = &id
	}
	m, err := h.kernel.Mandates.Grant(ctx, input)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.GrantMandateResponse{Mandate: toMandate(m, 0)}, nil
}

func (h *mandateHandler) RevokeMandate(ctx context.Context, req *mandatev1.RevokeMandateRequest) (*mandatev1.RevokeMandateResponse, error) {
	id, err := parseId(req.GetId(), "mandate id")
	if err != nil {
		return nil, err
	}
	if err := h.kernel.Mandates.Revoke(ctx, id); err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.RevokeMandateResponse{}, nil
}
