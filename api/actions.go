package api

import (
	"context"
	"encoding/json"
	"time"

	mandatev1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1/mandatev1connect"
	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type actionHandler struct{ *Handler }

var _ mandatev1connect.ActionServiceHandler = (*actionHandler)(nil)

func toAction(e actions.Action) *mandatev1.Action {
	return &mandatev1.Action{
		Id: e.Id.String(), Key: e.Key, Description: e.Description, Risk: e.Risk, MayExecute: e.MayExecute,
		WaitSeconds: int64(e.WaitLimit / time.Second), DeliverSeconds: int64(e.DeliverWithin / time.Second),
		SchemaJson: jsonText(e.Schema), Connector: e.ConnectorKind, ConnectorUrl: e.ConnectorUrl, HasSecret: e.HasSecret(),
		Department: e.Department, CreatedAt: ts(e.CreatedAt), UpdatedAt: ts(e.UpdatedAt), RemovedAt: tsPtr(e.RemovedAt),
	}
}

// ListActions lists the tenant's definitions and, after them, the builtin
// actions of an embedded host the tenant has not defined.
func (h *actionHandler) ListActions(ctx context.Context, req *mandatev1.ListActionsRequest) (*mandatev1.ListActionsResponse, error) {
	defined, err := h.kernel.Actions.List(ctx, req.GetIncludeRemoved())
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	resp := &mandatev1.ListActionsResponse{}
	seen := map[string]bool{}
	for _, e := range defined {
		resp.Actions = append(resp.Actions, toAction(e))
		seen[e.Key] = true
	}
	for _, code := range h.kernel.Builtin() {
		if seen[code.Key] {
			continue
		}
		resp.Actions = append(resp.Actions, &mandatev1.Action{
			Key: code.Key, Description: code.Description, Risk: code.Risk, MayExecute: code.MayExecute,
			WaitSeconds: int64(actions.DefaultWaitLimit / time.Second),
			SchemaJson:  jsonText(code.Schema), Connector: actions.ConnectorBuiltin, BuiltinOnly: true,
		})
	}
	return resp, nil
}

func (h *actionHandler) DefineAction(ctx context.Context, req *mandatev1.DefineActionRequest) (*mandatev1.DefineActionResponse, error) {
	input := actions.Input{
		Key: req.GetKey(), Description: req.GetDescription(), Risk: req.GetRisk(), MayExecute: req.GetMayExecute(),
		WaitLimit: time.Duration(req.GetWaitSeconds()) * time.Second, DeliverWithin: time.Duration(req.GetDeliverSeconds()) * time.Second,
		Connector: req.GetConnector(), Url: req.GetConnectorUrl(), RotateSecret: req.GetRotateSecret(), Department: req.GetDepartment(),
	}
	if req.GetSchemaJson() != "" {
		input.Schema = json.RawMessage(req.GetSchemaJson())
	}
	e, secret, err := h.kernel.Actions.Define(ctx, input)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.DefineActionResponse{Action: toAction(e), Secret: secret}, nil
}

func (h *actionHandler) RemoveAction(ctx context.Context, req *mandatev1.RemoveActionRequest) (*mandatev1.RemoveActionResponse, error) {
	if err := h.kernel.Actions.Remove(ctx, req.GetKey()); err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.RemoveActionResponse{}, nil
}
