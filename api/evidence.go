package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mandatev1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1"
	"github.com/andreas-lindfalk/pgmandate/gen/go/mandate/v1/mandatev1connect"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

type evidenceHandler struct{ *Handler }

var _ mandatev1connect.EvidenceServiceHandler = (*evidenceHandler)(nil)

// personNames maps the tenant's persons and agents to names, for the chain.
func (h *Handler) personNames(ctx context.Context) (map[uuid.UUID]string, error) {
	names := map[uuid.UUID]string{}
	persons, err := h.kernel.Tenants.ListPersons(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range persons {
		names[p.Id] = p.Name
	}
	agentsList, err := h.kernel.Agents.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range agentsList {
		names[a.Id] = a.Name
	}
	return names, nil
}

func toEvent(e events.Stored, names map[uuid.UUID]string) *mandatev1.Event {
	out := &mandatev1.Event{Seq: e.Seq, Type: e.Type, ActorKind: e.ActorKind, ActorId: idText(e.ActorId), SubjectId: idText(e.SubjectId), PayloadJson: string(e.Payload), CreatedAt: ts(e.CreatedAt)}
	if e.ActorId != nil {
		out.ActorName = names[*e.ActorId]
	}
	if e.RunId != nil {
		out.RunId = *e.RunId
	}
	return out
}

func (h *evidenceHandler) ListEvents(ctx context.Context, req *mandatev1.ListEventsRequest) (*mandatev1.ListEventsResponse, error) {
	var subject *uuid.UUID
	if req.GetSubjectId() != "" {
		id, err := parseId(req.GetSubjectId(), "subject id")
		if err != nil {
			return nil, err
		}
		subject = &id
	}
	names, err := h.personNames(ctx)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	var list []events.Stored
	if err := db.InTx(ctx, h.kernel.Pool(), func(tx pgx.Tx) error {
		var err error
		list, err = events.List(ctx, tx, subject, int(req.GetLimit()))
		return err
	}); err != nil {
		return nil, fault.ToConnect(err)
	}
	resp := &mandatev1.ListEventsResponse{}
	for _, e := range list {
		resp.Events = append(resp.Events, toEvent(e, names))
	}
	return resp, nil
}

func (h *evidenceHandler) VerifyChain(ctx context.Context, _ *mandatev1.VerifyChainRequest) (*mandatev1.VerifyChainResponse, error) {
	var count int64
	err := db.InTx(ctx, h.kernel.Pool(), func(tx pgx.Tx) error {
		var err error
		count, err = events.Verify(ctx, tx)
		return err
	})
	if errors.Is(err, events.ErrBroken) {
		return &mandatev1.VerifyChainResponse{Ok: false, Events: count, Error: err.Error()}, nil
	}
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.VerifyChainResponse{Ok: true, Events: count}, nil
}

func (h *evidenceHandler) ExportChain(ctx context.Context, _ *mandatev1.ExportChainRequest) (*mandatev1.ExportChainResponse, error) {
	var chain []events.Exported
	if err := db.InTx(ctx, h.kernel.Pool(), func(tx pgx.Tx) error {
		var err error
		chain, err = events.Export(ctx, tx)
		return err
	}); err != nil {
		return nil, fault.ToConnect(err)
	}
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	for _, e := range chain {
		if err := encoder.Encode(e); err != nil {
			return nil, fault.ToConnect(err)
		}
	}
	return &mandatev1.ExportChainResponse{Jsonl: out.String()}, nil
}

func (h *evidenceHandler) ExportDecisions(ctx context.Context, _ *mandatev1.ExportDecisionsRequest) (*mandatev1.ExportDecisionsResponse, error) {
	list, err := h.kernel.Proposals.Decided(ctx)
	if err != nil {
		return nil, fault.ToConnect(err)
	}
	var out strings.Builder
	if err := proposals.WriteDecisions(&out, list); err != nil {
		return nil, fault.ToConnect(err)
	}
	return &mandatev1.ExportDecisionsResponse{Jsonl: out.String()}, nil
}
