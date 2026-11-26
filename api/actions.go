package api

import (
	"context"
	"encoding/json"
	"math"
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
		SchemaJson: jsonText(e.Schema), Connector: e.ConnectorKind, ConnectorUrl: e.ConnectorUrl, Audience: e.ConnectorAudience,
		Department: e.Department, CreatedAt: ts(e.CreatedAt), UpdatedAt: ts(e.UpdatedAt), RemovedAt: tsPtr(e.RemovedAt),
	}
}

// ListActions lists the tenant's definitions and, after them, the builtin
// actions of an embedded host the tenant has not defined. A removed
// definition hides its builtin, as it does for proposing, even when the
// request leaves removed definitions out.
func (h *actionHandler) ListActions(ctx context.Context, req *mandatev1.ListActionsRequest) (*mandatev1.ListActionsResponse, error) {
	defined, err := h.kernel.Actions.List(ctx, true)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	resp := &mandatev1.ListActionsResponse{}
	seen := map[string]bool{}
	for _, e := range defined {
		seen[e.Key] = true
		if e.RemovedAt != nil && !req.GetIncludeRemoved() {
			continue
		}
		resp.Actions = append(resp.Actions, toAction(e))
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
		WaitLimit: seconds(req.GetWaitSeconds()), DeliverWithin: seconds(req.GetDeliverSeconds()),
		Connector: req.GetConnector(), Url: req.GetConnectorUrl(), Department: req.GetDepartment(),
	}
	if req.GetSchemaJson() != "" {
		input.Schema = json.RawMessage(req.GetSchemaJson())
	}
	e, err := h.kernel.Actions.Define(ctx, input)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.DefineActionResponse{Action: toAction(e)}, nil
}

// seconds converts a request's seconds to a duration that saturates
// instead of wrapping around: a count too large for a Duration would
// otherwise wrap to any value, 0 (the default) among them, and pass
// Define's range check. Saturated, it is out of range, and Define refuses
// it with its own message.
func seconds(n int64) time.Duration {
	const most = int64(math.MaxInt64 / time.Second)
	switch {
	case n > most:
		return math.MaxInt64
	case n < -most:
		return math.MinInt64
	}
	return time.Duration(n) * time.Second
}

func (h *actionHandler) RemoveAction(ctx context.Context, req *mandatev1.RemoveActionRequest) (*mandatev1.RemoveActionResponse, error) {
	if err := h.kernel.Actions.Remove(ctx, req.GetKey()); err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.RemoveActionResponse{}, nil
}
