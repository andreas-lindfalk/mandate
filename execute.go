package pgmandate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// Skip reasons, as written to the chain when an execution leaves the
// proposal for a person. They are the modules' own, under one name here.
const (
	SkipNotExecutable   = proposals.SkipNotExecutable
	SkipPayloadInvalid  = proposals.SkipPayloadInvalid
	SkipActionChanged   = proposals.SkipActionChanged
	SkipFlagged         = proposals.SkipFlagged
	SkipNoMandate       = mandates.SkipNoMandate
	SkipRiskAbove       = mandates.SkipRiskAbove
	SkipBudgetSpent     = mandates.SkipBudgetSpent
	SkipConditionUnmet  = mandates.SkipConditionUnmet
	SkipAgentRevoked    = proposals.SkipAgentRevoked
	SkipActionFailed    = proposals.SkipActionFailed
	SkipExecutionFailed = proposals.SkipExecutionFailed
	// SkipActionRemoved is not written to the chain; it is the reason the
	// API gives for a pending proposal whose action is no longer registered.
	SkipActionRemoved = proposals.SkipActionRemoved
	// SkipRefused is the prefix when the database refused the execution on
	// its own rules, such as a mandate revoked between the check and the
	// write.
	SkipRefused = proposals.SkipRefused
)

// ExecuteProposal carries a proposal out as the system when a mandate
// allows it now, in one transaction: the mandates are locked and read, the
// budget is counted, the status changes under the database's own checks,
// and the action is applied. Anything that does not hold leaves the proposal
// for a person and says why in the chain (proposal.execution_skipped). A
// decided proposal is left alone.
func (k *Kernel) ExecuteProposal(ctx context.Context, tenantId, proposalId uuid.UUID) error {
	ctx = actor.AsSystem(ctx, tenantId)
	// Execute checks under its own locks that the proposal is there and
	// pending, and records a revoked agent as agent_revoked.
	done, skipped, err := k.Proposals.Execute(ctx, proposalId, k.basisFor)
	switch {
	case err == nil && skipped != "":
		slog.InfoContext(ctx, "execution skipped", "tenant", tenantId, "proposal", proposalId, "reason", skipped)
		return nil
	case err == nil && done.DryRun:
		slog.InfoContext(ctx, "a mandate would allow the dry run; nothing was done", "tenant", tenantId, "proposal", proposalId)
		return nil
	case err == nil:
		slog.InfoContext(ctx, "proposal executed under a mandate", "tenant", tenantId, "proposal", proposalId)
		return nil
	}
	if errors.Is(err, proposals.ErrActionFailed) {
		if fault.CodeOf(err) == fault.Internal {
			// The action hit something transient, as far as anyone knows:
			// the job retries, and the last attempt records the failure.
			return fmt.Errorf("execute proposal %s: %w", proposalId, err)
		}
		// The action refused on its own rules (a closed ticket, a missing
		// row): retrying would only hit the same rule. A person decides.
		slog.WarnContext(ctx, "the action refused the execution", "proposal", proposalId, "error", err)
		return k.Proposals.RecordSkip(ctx, proposalId, SkipActionFailed)
	}
	switch code := fault.CodeOf(err); code {
	case fault.Internal:
		// A database or action failure: the job retries. The proposal is
		// still pending either way.
		return fmt.Errorf("execute proposal %s: %w", proposalId, err)
	case fault.FailedPrecondition, fault.NotFound:
		// A person decided it meanwhile, or its deadline passed: nothing to
		// explain, and nobody to tell that it waits.
		slog.InfoContext(ctx, "proposal was decided or expired before its execution", "proposal", proposalId)
		return nil
	default:
		// The database refused on its own rules. Not a job failure, and the
		// reason in the chain is a code, never the error text.
		slog.WarnContext(ctx, "the database left the proposal for a person", "proposal", proposalId, "error", err)
		return k.Proposals.RecordSkip(ctx, proposalId, SkipRefused+fault.Name(code))
	}
}

// basisFor asks the mandates, inside the execution transaction, whether one
// allows the proposal now.
func (k *Kernel) basisFor(ctx context.Context, tx pgx.Tx, proposal Proposal) (ExecutionBasis, string, error) {
	resolution, err := k.Mandates.Resolve(ctx, tx, proposal.AgentId, proposal.Action, proposal.Risk, proposal.Payload)
	if err != nil {
		return ExecutionBasis{}, "", err
	}
	if resolution.Reason != "" {
		return ExecutionBasis{}, resolution.Reason, nil
	}
	m := resolution.Mandate
	return ExecutionBasis{
		MandateId: m.Id, PolicyRef: m.PolicyRef, MaxRisk: m.MaxRisk,
		Budgets:   []proposals.Budget{{Kind: proposals.BudgetCount, Limit: m.BudgetCount, Spent: resolution.Spent}},
		Condition: m.Condition,
	}, "", nil
}
