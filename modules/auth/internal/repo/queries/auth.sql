-- name: InsertSession :exec
INSERT INTO mandate.sessions (token_hash, email, name, expires_at) VALUES (@token_hash, @email, @name, @expires_at);

-- name: GetSession :one
SELECT email, name FROM mandate.sessions WHERE token_hash = @token_hash AND expires_at > now();

-- name: TouchSession :exec
UPDATE mandate.sessions SET last_seen_at = now()
WHERE token_hash = @token_hash AND (last_seen_at IS NULL OR last_seen_at < now() - interval '1 minute');

-- name: DeleteSession :exec
DELETE FROM mandate.sessions WHERE token_hash = @token_hash;

-- name: DeleteExpiredSessions :execrows
DELETE FROM mandate.sessions WHERE expires_at < now();
