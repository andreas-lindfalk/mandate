-- name: InsertDelivery :exec
INSERT INTO mandate.deliveries (id, tenant_id, proposal_id, action_key, url)
VALUES (@id, @tenant_id, @proposal_id, @action_key, @url);

-- name: GetDelivery :one
SELECT * FROM mandate.deliveries WHERE id = @id;

-- name: GetDeliveryForUpdate :one
SELECT * FROM mandate.deliveries WHERE id = @id FOR UPDATE;

-- name: GetDeliveryByProposal :one
SELECT * FROM mandate.deliveries WHERE proposal_id = @proposal_id;

-- name: ListDeliveriesByProposals :many
SELECT * FROM mandate.deliveries WHERE proposal_id = ANY(@proposal_ids::uuid[]);

-- name: MarkAttempt :exec
UPDATE mandate.deliveries SET attempts = attempts + 1, last_status = sqlc.narg('last_status'), last_error = sqlc.narg('last_error')
WHERE id = @id;

-- name: MarkDelivered :exec
UPDATE mandate.deliveries SET status = 'delivered', attempts = attempts + 1, last_status = @last_status::int, last_error = NULL,
    result = sqlc.narg('result'), delivered_at = now()
WHERE id = @id;

-- name: RetryDelivery :exec
UPDATE mandate.deliveries SET status = 'pending', failed_at = NULL, url = @url WHERE id = @id;

-- name: ConnectorUrl :one
SELECT connector_url FROM mandate.actions
WHERE key = @key AND connector_kind = 'webhook' AND removed_at IS NULL AND connector_url IS NOT NULL;

-- name: MarkFailed :exec
UPDATE mandate.deliveries SET status = 'failed', attempts = attempts + 1, last_status = sqlc.narg('last_status'), last_error = sqlc.narg('last_error'),
    result = sqlc.narg('result'), failed_at = now()
WHERE id = @id;

-- name: RetargetPending :many
UPDATE mandate.deliveries SET url = @url
WHERE action_key = @action_key AND status = 'pending' AND url <> @url
RETURNING id, proposal_id;

-- name: ConnectorOf :one
SELECT url::text, secret::bytea FROM mandate.connector_of(@action_key::text);

-- name: MoveDelivery :exec
UPDATE mandate.deliveries SET url = @url WHERE id = @id;
