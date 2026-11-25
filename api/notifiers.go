package api

import (
	"context"

	mandatev1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1/mandatev1connect"
	"github.com/andreas-lindfalk/pgmandate/modules/notifications"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type notifierHandler struct{ *Handler }

var _ mandatev1connect.NotifierServiceHandler = (*notifierHandler)(nil)

func toNotifier(n *notifications.Notifier) *mandatev1.Notifier {
	if n == nil {
		return nil
	}
	return &mandatev1.Notifier{Id: n.Id.String(), Kind: n.Kind, CreatedBy: idText(n.CreatedBy), CreatedAt: ts(n.CreatedAt)}
}

func (h *notifierHandler) GetNotifier(ctx context.Context, _ *mandatev1.GetNotifierRequest) (*mandatev1.GetNotifierResponse, error) {
	n, err := h.kernel.Notifications.Get(ctx)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.GetNotifierResponse{Notifier: toNotifier(n)}, nil
}

func (h *notifierHandler) SetNotifier(ctx context.Context, req *mandatev1.SetNotifierRequest) (*mandatev1.SetNotifierResponse, error) {
	n, err := h.kernel.Notifications.Set(ctx, req.GetKind(), req.GetUrl())
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.SetNotifierResponse{Notifier: toNotifier(&n)}, nil
}

func (h *notifierHandler) TestNotifier(ctx context.Context, _ *mandatev1.TestNotifierRequest) (*mandatev1.TestNotifierResponse, error) {
	if err := h.kernel.Notifications.SendTest(ctx); err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.TestNotifierResponse{}, nil
}

func (h *notifierHandler) RemoveNotifier(ctx context.Context, _ *mandatev1.RemoveNotifierRequest) (*mandatev1.RemoveNotifierResponse, error) {
	if err := h.kernel.Notifications.Remove(ctx); err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.RemoveNotifierResponse{}, nil
}
