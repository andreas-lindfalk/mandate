-- name: InsertDelivery :one
-- The trigger sets not_after from the proposal's decision and checks the
-- address and audience are the action's own.
INSERT INTO mandate.deliveries (id, tenant_id, proposal_id, action_key, url, audience, not_after)
VALUES (@id, @tenant_id, @proposal_id, @action_key, @url, @audience, now())
RETURNING not_after;

-- name: GetDelivery :one
SELECT * FROM mandate.deliveries WHERE id = @id;

-- name: GetDeliveryForUpdate :one
SELECT * FROM mandate.deliveries WHERE id = @id FOR UPDATE;

-- name: GetDeliveryByProposal :one
SELECT * FROM mandate.deliveries WHERE proposal_id = @proposal_id;

-- name: GetDeliveryByProposalForUpdate :one
SELECT * FROM mandate.deliveries WHERE proposal_id = @proposal_id FOR UPDATE;

-- name: ListDeliveriesByProposals :many
SELECT * FROM mandate.deliveries WHERE proposal_id = ANY(@proposal_ids::uuid[]);

-- name: ConnectorOf :one
-- An action's address and audience as they are now.
SELECT connector_url::text AS url, connector_audience::text AS audience FROM mandate.actions
WHERE key = @key AND connector_kind = 'webhook' AND removed_at IS NULL;

-- name: DbNow :one
SELECT now()::timestamptz;

-- name: OpenAttempt :one
-- The guard refuses it too late, or over an attempt still open, and sets
-- attempt_open_at. The time returned is the database's, for the signature.
UPDATE mandate.deliveries SET attempts_started = attempts_started + 1
WHERE id = @id AND status = 'pending'
RETURNING attempt_open_at::timestamptz;

-- name: CloseUnsent :execrows
-- The attempt is proven never to have reached the receiver.
UPDATE mandate.deliveries
SET attempts_unsent = attempts_unsent + 1, attempt_open_at = NULL, last_status = sqlc.narg('last_status'), last_error = sqlc.narg('last_error')
WHERE id = @id AND status = 'pending' AND attempt_open_at IS NOT NULL;

-- name: CloseOpen :execrows
-- The attempt ended without an answer that settles anything: it stays
-- counted as possibly sent.
UPDATE mandate.deliveries
SET attempt_open_at = NULL, last_status = sqlc.narg('last_status'), last_error = sqlc.narg('last_error')
WHERE id = @id AND status = 'pending' AND attempt_open_at IS NOT NULL;

-- name: RecordAnswer :execrows
-- A receiver's explicit answer, with the attempt it answers. Nothing when
-- the attempt is no longer open: the answer came too late to count.
UPDATE mandate.deliveries
SET status = @status, result = sqlc.narg('result'), attempt_open_at = NULL, last_status = @last_status::int, last_error = NULL
WHERE id = @id AND status = 'pending' AND attempt_open_at IS NOT NULL;

-- name: Finish :one
-- Ends a waiting delivery with no attempt open: unknown if an attempt was
-- possibly sent, otherwise not sent.
UPDATE mandate.deliveries
SET status = CASE WHEN attempts_started > attempts_unsent THEN 'unknown' ELSE 'not_sent' END, last_error = sqlc.narg('reason')
WHERE id = @id AND status = 'pending' AND attempt_open_at IS NULL
RETURNING status;

-- name: CloseStale :many
-- Attempts left open by a crash: closed, and still counted as possibly sent.
UPDATE mandate.deliveries SET attempt_open_at = NULL
WHERE status = 'pending' AND attempt_open_at <= now() - mandate.delivery_stale()
RETURNING id, proposal_id;

-- name: ListDueToFinish :many
-- Waiting deliveries past the last moment an attempt may open, with none
-- open, locked for the sweep.
SELECT id FROM mandate.deliveries
WHERE status = 'pending' AND attempt_open_at IS NULL AND not_after - mandate.delivery_timeout() <= now()
ORDER BY not_after, id
LIMIT @max_rows
FOR UPDATE SKIP LOCKED;

-- name: MoveDelivery :exec
UPDATE mandate.deliveries SET url = @url, audience = @audience WHERE id = @id;

-- name: RetargetWaiting :many
-- An owner moved the action: its waiting deliveries that were never
-- possibly sent move along. One possibly sent stays where it was sent.
UPDATE mandate.deliveries SET url = @url, audience = @audience
WHERE action_key = @action_key AND status = 'pending' AND attempts_started = attempts_unsent AND attempt_open_at IS NULL
    AND (url <> @url OR audience <> @audience)
RETURNING id, proposal_id;

-- name: SendAgain :exec
UPDATE mandate.deliveries SET status = 'pending', url = @url, audience = @audience WHERE id = @id;

-- name: Settle :exec
UPDATE mandate.deliveries SET status = @status WHERE id = @id;
