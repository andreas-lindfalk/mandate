package api

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	mandatev1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1/mandatev1connect"
	"github.com/andreas-lindfalk/pgmandate/modules/deliveries"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type proposalHandler struct{ *Handler }

var _ mandatev1connect.ProposalServiceHandler = (*proposalHandler)(nil)

func toBasis(b *proposals.ExecutionBasis) *mandatev1.ExecutionBasis {
	if b == nil {
		return nil
	}
	return &mandatev1.ExecutionBasis{MandateId: b.MandateId.String(), PolicyRef: b.PolicyRef, MaxRisk: b.MaxRisk, BudgetUnits: b.BudgetUnits, SpentUnits: b.SpentUnits, CostUnits: b.CostUnits}
}

func toDelivery(d deliveries.Delivery) *mandatev1.Delivery {
	return &mandatev1.Delivery{
		Id: d.Id.String(), Status: d.Status, Attempts: int32(d.Attempts), Url: d.Url, LastError: d.LastError, //nolint:gosec // a small count
		DeliveredAt: tsPtr(d.DeliveredAt), FailedAt: tsPtr(d.FailedAt), ResultJson: jsonText(d.Result), MaxAttempts: deliveries.MaxAttempts,
	}
}

func toProposal(p proposals.Proposal, agentName, skipReason string, delivery *deliveries.Delivery) *mandatev1.Proposal {
	out := &mandatev1.Proposal{
		Id: p.Id.String(), AgentId: p.AgentId.String(), AgentName: agentName, RunId: p.RunId, Action: p.Action, Risk: p.Risk, Title: p.Title,
		PayloadJson: string(p.Payload), CostUnits: p.CostUnits, Flags: p.Flags, Status: p.Status, Edited: p.Edited, DecidedBy: idText(p.DecidedBy),
		DecidedAt: tsText(p.DecidedAt), DecisionNote: p.DecisionNote, CreatedAt: tsText(&p.CreatedAt), ExecutionBasis: toBasis(p.ExecutionBasis),
		SkipReason: skipReason, ProposedPayloadJson: string(p.ProposedPayload),
	}
	if delivery != nil {
		out.Delivery = toDelivery(*delivery)
	}
	return out
}

// enrich adds agent names, skip reasons and deliveries to proposals.
func (h *proposalHandler) enrich(ctx context.Context, list []proposals.Proposal) ([]*mandatev1.Proposal, error) {
	agentsList, err := h.kernel.Agents.List(ctx)
	if err != nil {
		return nil, err
	}
	names := map[uuid.UUID]string{}
	for _, a := range agentsList {
		names[a.Id] = a.Name
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.Id)
	}
	delivered, err := h.kernel.Deliveries.ByProposals(ctx, ids)
	if err != nil {
		return nil, err
	}
	skips, err := h.kernel.Proposals.SkipReasons(ctx, ids)
	if err != nil {
		return nil, err
	}
	actionsByKey := map[string]*proposals.Action{}
	out := make([]*mandatev1.Proposal, 0, len(list))
	for _, p := range list {
		reason := ""
		if p.Status == proposals.StatusPending {
			if r, found := skips[p.Id]; found {
				reason = r
			} else {
				action, cached := actionsByKey[p.Action]
				if !cached {
					if found, ok, err := h.kernel.Lookup(ctx, p.Action); err != nil {
						return nil, err
					} else if ok {
						action = &found
					}
					actionsByKey[p.Action] = action
				}
				if action != nil {
					reason = proposals.WhyNotCandidate(*action, p)
				} else {
					reason = "action_removed"
				}
			}
		}
		var d *deliveries.Delivery
		if found, ok := delivered[p.Id]; ok {
			d = &found
		}
		out = append(out, toProposal(p, names[p.AgentId], reason, d))
	}
	return out, nil
}

func (h *proposalHandler) ListProposals(ctx context.Context, req *mandatev1.ListProposalsRequest) (*mandatev1.ListProposalsResponse, error) {
	list, err := h.kernel.Proposals.List(ctx, proposals.Filter{Status: req.GetStatus(), OldestFirst: req.GetOldestFirst(), Limit: int(req.GetLimit())})
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	total, err := h.kernel.Proposals.Count(ctx, req.GetStatus())
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	out, err := h.enrich(ctx, list)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.ListProposalsResponse{Proposals: out, Total: total}, nil
}

func (h *proposalHandler) GetProposal(ctx context.Context, req *mandatev1.GetProposalRequest) (*mandatev1.GetProposalResponse, error) {
	id, err := parseId(req.GetId(), "proposal id")
	if err != nil {
		return nil, err
	}
	p, err := h.kernel.Proposals.Get(ctx, id)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	out, err := h.enrich(ctx, []proposals.Proposal{p})
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	history, err := h.kernel.Proposals.History(ctx, id)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	names, err := h.personNames(ctx)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	resp := &mandatev1.GetProposalResponse{Proposal: out[0]}
	if p.Status == proposals.StatusPending {
		action, found, err := h.kernel.Lookup(ctx, p.Action)
		if err != nil {
			return nil, fault.ToConnect(err)
		}
		if found {
			resp.Proposal.ApprovalRisk = proposals.RiskAtApproval(action, p)
		}
	}
	for _, e := range history {
		resp.History = append(resp.History, toEvent(e, names))
	}
	return resp, nil
}

func (h *proposalHandler) ApproveProposal(ctx context.Context, req *mandatev1.ApproveProposalRequest) (*mandatev1.ApproveProposalResponse, error) {
	id, err := parseId(req.GetId(), "proposal id")
	if err != nil {
		return nil, err
	}
	decision := proposals.Decision{Title: req.GetTitle(), Confirm: req.GetConfirm(), Note: req.GetNote()}
	if req.GetPayloadJson() != "" {
		decision.Payload = json.RawMessage(req.GetPayloadJson())
	}
	p, err := h.kernel.Proposals.Approve(ctx, id, decision)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	out, err := h.enrich(ctx, []proposals.Proposal{p})
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.ApproveProposalResponse{Proposal: out[0]}, nil
}

func (h *proposalHandler) RedeliverProposal(ctx context.Context, req *mandatev1.RedeliverProposalRequest) (*mandatev1.RedeliverProposalResponse, error) {
	id, err := parseId(req.GetId(), "proposal id")
	if err != nil {
		return nil, err
	}
	if _, err := h.kernel.Deliveries.Redeliver(ctx, id); err != nil {
		return nil, fault.ToConnect(err)
	}
	p, err := h.kernel.Proposals.Get(ctx, id)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	out, err := h.enrich(ctx, []proposals.Proposal{p})
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.RedeliverProposalResponse{Proposal: out[0]}, nil
}

func (h *proposalHandler) RejectProposal(ctx context.Context, req *mandatev1.RejectProposalRequest) (*mandatev1.RejectProposalResponse, error) {
	id, err := parseId(req.GetId(), "proposal id")
	if err != nil {
		return nil, err
	}
	p, err := h.kernel.Proposals.Reject(ctx, id, req.GetNote())
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	out, err := h.enrich(ctx, []proposals.Proposal{p})
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.RejectProposalResponse{Proposal: out[0]}, nil
}
