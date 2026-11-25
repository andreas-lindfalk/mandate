-- name: InsertSession :exec
INSERT INTO mandate.sessions (token_hash, email, name, issuer, subject, person_id, expires_at)
VALUES (@token_hash, @email, @name, sqlc.narg('issuer'), sqlc.narg('subject'), sqlc.narg('person_id'), @expires_at);

-- name: GetSession :one
SELECT email, name, issuer, subject, person_id FROM mandate.sessions WHERE token_hash = @token_hash AND expires_at > now();

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

-- name: HasTenants :one
SELECT EXISTS (SELECT 1 FROM mandate.all_tenant_ids())::boolean;

-- name: VoidSetupLinks :exec
-- Unused setup links, when a new one is made.
DELETE FROM mandate.sign_in_links WHERE purpose = 'setup' AND used_at IS NULL;

-- name: InsertLink :exec
-- Its lifetime is counted on the database's clock, which the guard checks.
INSERT INTO mandate.sign_in_links (token_hash, purpose, tenant_id, person_id, expires_at)
VALUES (@token_hash, @purpose, sqlc.narg('tenant_id'), sqlc.narg('person_id'), now() + make_interval(secs => @lifetime_seconds::int));

-- name: GetLinkForUpdate :one
SELECT purpose, tenant_id, person_id, expires_at, used_at FROM mandate.sign_in_links WHERE token_hash = @token_hash FOR UPDATE;

-- name: UseLink :execrows
-- Nothing when it is used or expired, by the database's clock.
UPDATE mandate.sign_in_links SET used_at = now() WHERE token_hash = @token_hash AND used_at IS NULL AND expires_at > now();

-- name: DeleteExpiredLinks :execrows
DELETE FROM mandate.sign_in_links WHERE expires_at <= now();

-- name: LockSetup :exec
-- One setup at a time across every server: making a setup link and using
-- one both take it, so two links never both make a tenant.
SELECT pg_advisory_xact_lock(hashtextextended('mandate.setup', 0));
