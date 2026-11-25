-- name: InsertProposal :one
INSERT INTO mandate.proposals (id, tenant_id, agent_id, action, risk, title, payload, flags, idempotency_key, request_hash,
    rationale, evidence, provenance)
VALUES (@id, @tenant_id, @agent_id, @action, @risk, @title, @payload, @flags::text[], @idempotency_key, @request_hash,
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
    AND (sqlc.narg('dry_run')::boolean IS NULL OR dry_run = sqlc.narg('dry_run')::boolean)
ORDER BY (CASE WHEN @oldest_first::boolean THEN created_at END) ASC, created_at DESC, id
LIMIT @max_rows;

-- name: CountProposals :one
SELECT count(*) FROM mandate.proposals
WHERE (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
    AND (sqlc.narg('dry_run')::boolean IS NULL OR dry_run = sqlc.narg('dry_run')::boolean);

-- name: ListDecided :many
-- What people and mandates decided: not what was withdrawn or expired.
SELECT * FROM mandate.proposals WHERE status IN ('approved', 'rejected', 'executed') ORDER BY decided_at, id;

-- name: DecideProposal :exec
UPDATE mandate.proposals
SET status = @status, title = @title, payload = @payload, risk = @risk, decision_note = sqlc.narg('decision_note')
WHERE id = @id;

-- name: ExecuteProposal :exec
-- executed for a live proposal, would_allow for a dry run: the same basis,
-- held to the same rules, and only executed counts against the budget.
UPDATE mandate.proposals SET status = CASE WHEN dry_run THEN 'would_allow' ELSE 'executed' END, execution_basis = @basis WHERE id = @id;

-- name: DismissProposal :execrows
UPDATE mandate.proposals SET status = 'dismissed', decision_note = sqlc.narg('note') WHERE id = @id AND status = 'pending' AND dry_run;

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

-- name: ListDueForExpiry :many
-- Waiting proposals past their deadline, locked for the sweep; a row another
-- transaction holds is left for the next sweep.
SELECT id FROM mandate.proposals WHERE status = 'pending' AND expires_at <= now()
ORDER BY expires_at, id
LIMIT @max_rows
FOR UPDATE SKIP LOCKED;

-- name: ExpireProposal :execrows
UPDATE mandate.proposals SET status = 'expired' WHERE id = @id AND status = 'pending';

-- name: PastDeadline :one
-- Whether a proposal's deadline has passed by the database's clock, the
-- same now() the update trigger compares with.
SELECT (now() >= expires_at)::boolean FROM mandate.proposals WHERE id = @id;
