-- name: InsertAgent :exec
INSERT INTO mandate.agents (id, tenant_id, name, department, mode) VALUES (@id, @tenant_id, @name, sqlc.narg('department'), @mode);

-- name: GetAgent :one
SELECT * FROM mandate.agents WHERE id = @id;

-- name: ListAgents :many
SELECT * FROM mandate.agents ORDER BY revoked_at IS NOT NULL, created_at, id;

-- name: RevokeAgent :execrows
UPDATE mandate.agents SET revoked_at = now() WHERE id = @id AND revoked_at IS NULL;

-- name: InsertToken :exec
INSERT INTO mandate.agent_tokens (id, tenant_id, agent_id, token_hash, hint, created_at, expires_at)
VALUES (@id, @tenant_id, @agent_id, @token_hash, @hint, @created_at, @expires_at);

-- name: ListTokens :many
SELECT id, agent_id, hint, created_at, expires_at, last_used_at, revoked_at
FROM mandate.agent_tokens
WHERE agent_id = @agent_id
ORDER BY revoked_at IS NOT NULL, created_at DESC;

-- name: GetToken :one
SELECT id, agent_id, revoked_at FROM mandate.agent_tokens WHERE id = @id;

-- name: RevokeToken :exec
UPDATE mandate.agent_tokens SET revoked_at = now() WHERE id = @id AND revoked_at IS NULL;

-- name: TouchToken :exec
UPDATE mandate.agent_tokens SET last_used_at = now()
WHERE id = @id AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: SetAgentLimits :execrows
UPDATE mandate.agents SET max_per_minute = @max_per_minute, max_per_hour = @max_per_hour WHERE id = @id AND revoked_at IS NULL;

-- name: SetAgentMode :execrows
-- Nothing when the agent is revoked or already in the mode.
UPDATE mandate.agents SET mode = @mode WHERE id = @id AND revoked_at IS NULL AND mode <> @mode;

-- name: GetAgentForUpdate :one
SELECT * FROM mandate.agents WHERE id = @id FOR UPDATE;
