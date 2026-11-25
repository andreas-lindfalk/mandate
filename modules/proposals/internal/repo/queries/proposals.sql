-- name: InsertProposal :one
INSERT INTO mandate.proposals (id, tenant_id, agent_id, action, risk, title, payload, cost_units, flags, idempotency_key, request_hash,
    rationale, evidence, provenance)
VALUES (@id, @tenant_id, @agent_id, @action, @risk, @title, @payload, @cost_units, @flags::text[], @idempotency_key, @request_hash,
    @rationale::text, @evidence::jsonb, @provenance::jsonb)
ON CONFLICT (tenant_id, agent_id, idempotency_key) DO NOTHING
RETURNING id;

-- name: GetByIdempotencyKey :one
SELECT * FROM mandate.proposals WHERE agent_id = @agent_id AND idempotency_key = @idempotency_key;

-- name: GetProposal :one
SELECT * FROM mandate.proposals WHERE id = @id;

-- name: GetProposalForUpdate :one
SELECT * FROM mandate.proposals WHERE id = @id FOR UPDATE;

-- name: ListProposals :many
SELECT * FROM mandate.proposals
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
    AND (sqlc.narg('agent_id')::uuid IS NULL OR agent_id = sqlc.narg('agent_id')::uuid)
    AND (sqlc.narg('run_id')::text IS NULL OR run_id = sqlc.narg('run_id')::text)
ORDER BY (CASE WHEN @oldest_first::boolean THEN created_at END) ASC, created_at DESC, id
LIMIT @max_rows;

-- name: CountProposals :one
SELECT count(*) FROM mandate.proposals
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text);

-- name: ListDecided :many
SELECT * FROM mandate.proposals WHERE status <> 'pending' ORDER BY decided_at, id;

-- name: DecideProposal :exec
UPDATE mandate.proposals
SET status = @status, title = @title, payload = @payload, risk = @risk, decision_note = sqlc.narg('decision_note')
WHERE id = @id;

-- name: ExecuteProposal :exec
UPDATE mandate.proposals SET status = 'executed', execution_basis = @basis WHERE id = @id;

-- name: LastSkips :many
-- The reason of the newest execution_skipped event per proposal.
SELECT DISTINCT ON (subject_id) subject_id::uuid AS subject_id, coalesce(payload->>'reason', '')::text AS reason
FROM mandate.events
WHERE type = 'proposal.execution_skipped' AND subject_id = ANY(@subject_ids::uuid[])
ORDER BY subject_id, seq DESC;

-- name: CountRecentApprovals :one
SELECT count(*) FROM mandate.events
WHERE type = 'proposal.approved' AND actor_id = @actor_id AND created_at > now() - interval '1 minute';

-- name: WithdrawProposal :execrows
UPDATE mandate.proposals SET status = 'withdrawn', decision_note = sqlc.narg('note') WHERE id = @id AND status = 'pending';
