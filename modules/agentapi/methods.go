package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/andreas-lindfalk/pgmandate/gen/go/mandate/agent/v1"
	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/deliveries"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

func (s *Service) ListActions(ctx context.Context, _ *agentv1.ListActionsRequest) (*agentv1.ListActionsResponse, error) {
	return serve(s, ctx, "list_actions", map[string]any{}, "", s.listActions)
}

func (s *Service) listActions(ctx context.Context) (*agentv1.ListActionsResponse, error) {
	live, err := s.host.LiveMandates(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.host.ActionsOf(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(live))
	for _, m := range live {
		ids = append(ids, m.Id)
	}
	spent, err := s.host.SpentCount(ctx, ids)
	if err != nil {
		return nil, err
	}
	resp := &agentv1.ListActionsResponse{
		// A fresh run id, for an agent whose host gave it none: a headless
		// run that reuses a run id would reuse its keys and get last run's
		// answers.
		SuggestedRunId: "run-" + uuid.NewString(),
		Actions:        []*agentv1.Action{},
	}
	for _, action := range list {
		view := &agentv1.Action{
			Key: action.Key, Description: action.Description, Risk: action.Risk,
			WaitSeconds: int32(waitLimit(action) / time.Second), //nolint:gosec // at most 30 days
			SchemaJson:  string(action.Schema),
		}
		best, unconditional := "", false
		for _, m := range live {
			if !m.Covers(action.Key) {
				continue
			}
			view.Mandates = append(view.Mandates, &agentv1.Mandate{
				Id: m.Id.String(), MaxRisk: m.MaxRisk, BudgetCount: int32(m.BudgetCount), //nolint:gosec // at most 10^9 by the schema
				Remaining: proto.Int32(int32(max(m.BudgetCount-spent[m.Id], 0))), PolicyRef: m.PolicyRef, //nolint:gosec // within the budget
				ValidUntil: timestamppb.New(m.ValidUntil), Condition: m.Condition, SharedWithOtherAgents: m.AgentId == nil,
			})
			if mandates.RiskRank(m.MaxRisk) > mandates.RiskRank(best) {
				best = m.MaxRisk
			}
		}
		// Whether a mandate without a condition allows the risk the line
		// below names: the action's, or for an assessed action the highest.
		named := action.Risk
		if action.Assess != nil {
			named = best
		}
		for _, m := range live {
			if m.Covers(action.Key) && m.Condition == "" && m.Allows(named) {
				unconditional = true
			}
		}
		// autonomous: a mandate can carry it out without a person.
		autonomous := false
		switch {
		case !action.MayExecute || (action.Risk == mandates.RiskHigh && action.Assess == nil):
			view.Autonomy = "never without a person"
		case !s.host.Executes():
			view.Autonomy = "a person decides (executions are not running here)"
		case best == "":
			view.Autonomy = "a person decides: no mandate covers it"
		case action.Assess != nil:
			autonomous = true
			view.Autonomy = fmt.Sprintf("the risk depends on the payload: carried out without a person when assessed at %s risk or below, within the mandate's budget; otherwise a person decides", best)
		case mandates.RiskRank(action.Risk) > mandates.RiskRank(best):
			view.Autonomy = fmt.Sprintf("a person decides: the action is %s risk and your mandate allows up to %s", action.Risk, best)
		default:
			autonomous = true
			view.Autonomy = fmt.Sprintf("carried out without a person, within the mandate's budget (risk up to %s)", best)
		}
		if autonomous && !unconditional {
			view.Autonomy += "; only when the payload meets the condition of one of the mandates listed, otherwise a person decides"
		}
		resp.Actions = append(resp.Actions, view)
	}
	// The agent's limits on proposals, which the database holds: past
	// them, a proposal is refused until the minute or hour passes.
	if a, ok := actor.From(ctx); ok {
		agent, err := s.host.AgentsService().Get(ctx, a.Id)
		if err != nil {
			return nil, err
		}
		resp.Limits = &agentv1.Limits{ProposalsPerMinute: int32(agent.MaxPerMinute), ProposalsPerHour: int32(agent.MaxPerHour)} //nolint:gosec // small limits
	}
	return resp, nil
}

func (s *Service) Propose(ctx context.Context, req *agentv1.ProposeRequest) (*agentv1.ProposeResponse, error) {
	arguments := map[string]any{
		"action": logged(req.GetAction(), actionPattern), "titleChars": utf8.RuneCountInString(req.GetTitle()), "payloadBytes": len(req.GetPayloadJson()),
		"runId": logged(req.GetRunId(), runPattern), "idempotencyKeyChars": utf8.RuneCountInString(req.GetIdempotencyKey()),
		"rationaleChars": utf8.RuneCountInString(req.GetRationale()), "evidenceItems": len(req.GetEvidence()),
	}
	return serve(s, ctx, "propose", arguments, req.GetRunId(), func(ctx context.Context) (*agentv1.ProposeResponse, error) {
		return s.propose(ctx, req)
	})
}

func (s *Service) propose(ctx context.Context, req *agentv1.ProposeRequest) (*agentv1.ProposeResponse, error) {
	if err := requireMatch(req.GetAction(), actionPattern, "action key", actionForm); err != nil {
		return nil, err
	}
	if req.GetRunId() != "" {
		if err := requireMatch(req.GetRunId(), runPattern, "run id", runForm); err != nil {
			return nil, err
		}
	}
	if req.GetIdempotencyKey() == "" {
		return nil, fault.New(fault.InvalidArgument, "idempotency_key is required: name this one request, and send the same key if you send it again")
	}
	payload := json.RawMessage(req.GetPayloadJson())
	if trimmed := bytes.TrimSpace(payload); len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, fault.New(fault.InvalidArgument, "the payload must be a JSON object")
	}
	evidence := make([]proposals.Evidence, 0, len(req.GetEvidence()))
	for _, e := range req.GetEvidence() {
		evidence = append(evidence, proposals.Evidence{Source: e.GetSource(), Ref: e.GetRef(), Summary: e.GetSummary()})
	}
	call, _ := callFrom(ctx)
	proposal, created, err := s.host.ProposalsService().Propose(ctx, proposals.Input{
		Action: req.GetAction(), Title: req.GetTitle(), Payload: payload, IdempotencyKey: req.GetIdempotencyKey(),
		Rationale: req.GetRationale(), Evidence: evidence, Provenance: provenance(req.GetProvenance(), call),
	})
	if err != nil {
		return nil, err
	}
	action, _, err := s.host.Lookup(ctx, proposal.Action)
	if err != nil {
		return nil, err
	}
	// When the proposal may be executed, the job runs right after the
	// commit: wait a moment, so the agent learns the decision, the delivery
	// and the result in this one call. A repeat that arrives while the
	// first call's job still runs waits for it too, or it would be told
	// that a person must decide.
	wait := time.Duration(0)
	if proposal.Status == proposals.StatusPending && s.host.Executes() && proposals.Candidate(action, proposal) {
		wait = outcomeWait
	}
	view, err := s.settled(ctx, proposal.Id, wait, true)
	if err != nil {
		return nil, err
	}
	resp := &agentv1.ProposeResponse{Proposal: view, Created: proto.Bool(created)}
	// Only the answer that made a dry run tells the agent to act itself.
	if created && proposal.DryRun {
		view.Next = DryRunNext
	}
	if !created {
		// A repeat must not read as something that just happened: a model
		// that reuses a key next week would otherwise think it acted.
		resp.Repeat = true
		// The rationale never changes, so it alone shows a different
		// account; the title may have been edited by the person who
		// approved it.
		resp.AccountIgnored = strings.TrimSpace(req.GetRationale()) != proposal.Rationale
		view.Outcome = "a repeat: this call did nothing new. The request you sent under this key at " + proposal.CreatedAt + " stands as follows: " + view.Outcome
		// What to do about the first proposal still holds, so it is kept.
		if view.Next != "" {
			view.Next = "This answers a request you already sent under this key. " + view.Next
		} else {
			view.Next = "If you meant to send the same request again, this is its answer. If this is a new request, propose it under a new idempotency_key that includes your run id."
		}
	}
	return resp, nil
}

// provenance is who wrote the proposal: the client's name and version when
// they are labels the log may keep, and what the agent says of itself. An
// MCP client's own introduction replaces what the request says.
func provenance(p *agentv1.Provenance, call Call) proposals.Provenance {
	// A label has no spaces; "claude opus" becomes claude-opus rather than
	// sinking the proposal it describes.
	label := func(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), " ", "-") }
	out := proposals.Provenance{Model: label(p.GetModel()), AgentVersion: label(p.GetAgentVersion())}
	client, version := p.GetClient(), p.GetClientVersion()
	if call.Client != "" || call.ClientVersion != "" {
		client, version = call.Client, call.ClientVersion
	}
	// A client may name itself with spaces, as "Visual Studio Code"; a
	// label has none. Anything else that is not a label is left out.
	if name := label(client); proposals.ProvenancePattern.MatchString(name) {
		out.Client = name
	}
	if proposals.ProvenancePattern.MatchString(version) {
		out.ClientVersion = version
	}
	return out
}

func (s *Service) GetProposal(ctx context.Context, req *agentv1.GetProposalRequest) (*agentv1.GetProposalResponse, error) {
	arguments := map[string]any{"proposalId": logged(req.GetProposalId(), uuidPattern), "waitSeconds": req.GetWaitSeconds()}
	return serve(s, ctx, "get_proposal", arguments, "", func(ctx context.Context) (*agentv1.GetProposalResponse, error) {
		if err := requireMatch(req.GetProposalId(), uuidPattern, "proposal id", uuidForm); err != nil {
			return nil, err
		}
		id := uuid.MustParse(req.GetProposalId())
		proposal, err := s.host.ProposalsService().Get(ctx, id)
		if err != nil {
			return nil, err
		}
		// An agent reads only its own proposals; the database separates
		// tenants, not agents.
		if a, _ := actor.From(ctx); proposal.AgentId != a.Id {
			return nil, fault.New(fault.NotFound, "proposal not found")
		}
		wait := min(max(time.Duration(req.GetWaitSeconds())*time.Second, 0), MaxWait)
		view, err := s.settled(ctx, id, wait, false)
		if err != nil {
			return nil, err
		}
		history, err := s.history(ctx, id)
		if err != nil {
			return nil, err
		}
		return &agentv1.GetProposalResponse{Proposal: view, History: history}, nil
	})
}

func (s *Service) ListProposals(ctx context.Context, req *agentv1.ListProposalsRequest) (*agentv1.ListProposalsResponse, error) {
	arguments := map[string]any{"status": logged(req.GetStatus(), statusPattern), "runId": logged(req.GetRunId(), runPattern), "limit": req.GetLimit()}
	return serve(s, ctx, "list_proposals", arguments, "", func(ctx context.Context) (*agentv1.ListProposalsResponse, error) {
		if req.GetStatus() != "" {
			if err := requireMatch(req.GetStatus(), statusPattern, "status", statusForm); err != nil {
				return nil, err
			}
		}
		if req.GetRunId() != "" {
			if err := requireMatch(req.GetRunId(), runPattern, "run id", runForm); err != nil {
				return nil, err
			}
		}
		// Not given is 20; past the contract's 100 is 100, so an agent that
		// asks for more gets as many as it may, not fewer than it asked.
		limit := min(int(req.GetLimit()), 100)
		if limit <= 0 {
			limit = 20
		}
		a, _ := actor.From(ctx)
		list, err := s.host.ProposalsService().List(ctx, proposals.Filter{Status: req.GetStatus(), AgentId: &a.Id, RunId: req.GetRunId(), Limit: limit})
		if err != nil {
			return nil, err
		}
		ids := make([]uuid.UUID, 0, len(list))
		for _, p := range list {
			ids = append(ids, p.Id)
		}
		delivered, err := s.host.DeliveriesOf(ctx, ids)
		if err != nil {
			return nil, err
		}
		// Why each waiting proposal waits, as get_proposal says it.
		reasons, err := s.host.WaitReasons(ctx, list)
		if err != nil {
			return nil, err
		}
		resp := &agentv1.ListProposalsResponse{Proposals: make([]*agentv1.Proposal, 0, len(list))}
		for _, p := range list {
			reason := reasons[p.Id]
			var d *deliveries.Delivery
			if found, ok := delivered[p.Id]; ok {
				d = &found
			}
			resp.Proposals = append(resp.Proposals, proposalView(p, reason, d))
		}
		return resp, nil
	})
}

func (s *Service) WithdrawProposal(ctx context.Context, req *agentv1.WithdrawProposalRequest) (*agentv1.WithdrawProposalResponse, error) {
	arguments := map[string]any{"proposalId": logged(req.GetProposalId(), uuidPattern), "reasonChars": utf8.RuneCountInString(req.GetReason())}
	return serve(s, ctx, "withdraw_proposal", arguments, "", func(ctx context.Context) (*agentv1.WithdrawProposalResponse, error) {
		if err := requireMatch(req.GetProposalId(), uuidPattern, "proposal id", uuidForm); err != nil {
			return nil, err
		}
		p, err := s.host.ProposalsService().Withdraw(ctx, uuid.MustParse(req.GetProposalId()), req.GetReason())
		if err != nil {
			return nil, err
		}
		return &agentv1.WithdrawProposalResponse{Proposal: proposalView(p, "", nil)}, nil
	})
}

// settled reads a proposal's state, waiting up to wait for it to settle:
// decided and no longer on its way to the connector. For a proposal that
// was just made, a recorded reason why it waits for a person settles it too.
func (s *Service) settled(ctx context.Context, id uuid.UUID, wait time.Duration, justProposed bool) (*agentv1.Proposal, error) {
	service := s.host.ProposalsService()
	deadline := time.Now().Add(wait)
	for {
		p, err := service.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		reason := ""
		recorded := false
		var delivery *deliveries.Delivery
		done := false
		if p.Status == proposals.StatusPending {
			if reason, recorded, err = service.SkipReason(ctx, id); err != nil {
				return nil, err
			}
			// Nobody can decide a proposal past its deadline any more, so
			// waiting would change nothing. Nobody decides a dry run, so a
			// read does not wait for one; a proposal just made waits for
			// its mandates' answer.
			done = p.PastDeadline || (justProposed && recorded) || (p.DryRun && !justProposed)
		} else {
			d, found, err := s.host.DeliveryOf(ctx, id)
			if err != nil {
				return nil, err
			}
			if found {
				delivery = &d
			}
			done = !found || d.Status != deliveries.StatusPending
		}
		if done || !time.Now().Before(deadline) {
			if p.Status == proposals.StatusPending && !recorded {
				// Nothing recorded: what rules it out, from the action.
				action, found, err := s.host.Lookup(ctx, p.Action)
				if err != nil {
					return nil, err
				}
				reason = proposals.WaitReason(action, found, p)
			}
			view := proposalView(p, reason, delivery)
			if p.Status == proposals.StatusPending && !p.PastDeadline && reason == mandates.SkipConditionUnmet {
				if view.Unmet, err = s.host.Unmet(ctx, p.AgentId, p.Action, p.Risk, p.Payload); err != nil {
					return nil, err
				}
			}
			return view, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(outcomePoll):
		}
	}
}

func (s *Service) history(ctx context.Context, subjectId uuid.UUID) ([]*agentv1.Event, error) {
	list, err := s.host.ProposalsService().History(ctx, subjectId)
	if err != nil {
		return nil, err
	}
	out := make([]*agentv1.Event, 0, len(list))
	for _, e := range list {
		event := &agentv1.Event{Seq: e.Seq, Type: e.Type, ActorKind: e.ActorKind, PayloadJson: string(e.Payload), CreatedAt: timestamppb.New(e.CreatedAt)}
		if e.RunId != nil {
			event.RunId = *e.RunId
		}
		out = append(out, event)
	}
	return out, nil
}

// waitLimit is how long a proposal for the action waits: its definition's,
// or the default for an action that exists only as code.
func waitLimit(action proposals.Action) time.Duration {
	if action.WaitLimit == 0 {
		return actions.DefaultWaitLimit
	}
	return action.WaitLimit
}
