-- name: InsertMandate :exec
INSERT INTO mandate.mandates (id, tenant_id, agent_id, actions, max_risk, budget_units, policy_ref, department, valid_from, valid_until)
VALUES (@id, @tenant_id, sqlc.narg('agent_id'), @actions::text[], @max_risk, @budget_units, @policy_ref, sqlc.narg('department'), @valid_from, @valid_until);

-- name: GetMandate :one
SELECT * FROM mandate.mandates WHERE id = @id;

-- name: ListMandates :many
SELECT * FROM mandate.mandates
WHERE @include_ended::boolean OR (revoked_at IS NULL AND valid_until > now())
ORDER BY created_at DESC, id;

-- name: RevokeMandate :execrows
UPDATE mandate.mandates SET revoked_at = now() WHERE id = @id AND revoked_at IS NULL;

-- name: ListLiveForAgent :many
-- Mandates that hold at the instant given, for the agent, locked so that two
-- executions under the same mandate count the budget one after the other.
SELECT * FROM mandate.mandates
WHERE (agent_id IS NULL OR agent_id = @agent_id::uuid)
    AND revoked_at IS NULL AND valid_from <= @at AND valid_until > @at
ORDER BY max_risk DESC, created_at, id
FOR UPDATE;

-- name: SpentUnits :one
-- What the executions under a mandate have cost this month, as the
-- database counts it (the same function the execution trigger uses).
SELECT mandate.spent_units(@mandate_id::uuid)::bigint;

-- name: SpentUnitsFor :many
SELECT id, mandate.spent_units(id)::bigint AS spent FROM mandate.mandates WHERE id = ANY(@ids::uuid[]);

-- name: AgentState :one
-- Whether the agent exists in the tenant in scope, and is live.
SELECT (revoked_at IS NULL)::boolean AS live FROM mandate.agents WHERE id = @id;
