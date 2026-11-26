-- name: InsertNotifier :exec
INSERT INTO mandate.notifiers (id, tenant_id, kind, url) VALUES (@id, @tenant_id, @kind, @url);

-- name: RemoveLiveNotifier :execrows
UPDATE mandate.notifiers SET removed_at = now() WHERE id = @id AND removed_at IS NULL;

-- name: LiveNotifier :one
SELECT id, kind, created_by, created_at FROM mandate.notifiers WHERE removed_at IS NULL;

-- name: HasLiveNotifier :one
SELECT EXISTS (SELECT 1 FROM mandate.notifiers WHERE removed_at IS NULL)::boolean;

-- name: NotifierOf :many
-- The live notifier with its address; only the system may ask.
SELECT kind::text AS kind, url::text AS url FROM mandate.notifier_of();
