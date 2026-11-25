-- name: InsertSession :exec
INSERT INTO mandate.sessions (token_hash, email, name, issuer, subject, expires_at)
VALUES (@token_hash, @email, @name, sqlc.narg('issuer'), sqlc.narg('subject'), @expires_at);

-- name: GetSession :one
SELECT email, name, issuer, subject FROM mandate.sessions WHERE token_hash = @token_hash AND expires_at > now();

-- name: TouchSession :exec
UPDATE mandate.sessions SET last_seen_at = now()
WHERE token_hash = @token_hash AND (last_seen_at IS NULL OR last_seen_at < now() - interval '1 minute');

-- name: DeleteSession :exec
DELETE FROM mandate.sessions WHERE token_hash = @token_hash;

-- name: DeleteExpiredSessions :execrows
DELETE FROM mandate.sessions WHERE expires_at < now();

-- name: BindPerson :execrows
-- Nothing when the person is bound already, or removed meanwhile.
UPDATE mandate.persons SET issuer = sqlc.narg('issuer'), subject = sqlc.narg('subject')
WHERE id = @id AND issuer IS NULL AND removed_at IS NULL;

-- name: PersonIdentity :one
SELECT issuer, subject, removed_at FROM mandate.persons WHERE id = @id;
