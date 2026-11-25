-- The connector's secret is written here and never read back: the app
-- role has no right to read it (see connector_of in the schema).

-- name: InsertAction :exec
INSERT INTO mandate.actions (id, tenant_id, key, description, risk, wait_limit, deliver_within, may_execute, schema, connector_kind, connector_url, connector_secret, department)
VALUES (@id, @tenant_id, @key, @description, @risk, make_interval(secs => @wait_seconds::bigint), make_interval(secs => @deliver_seconds::bigint), @may_execute, sqlc.narg('schema'), @connector_kind,
        sqlc.narg('connector_url'), sqlc.narg('connector_secret'), sqlc.narg('department'));

-- name: UpdateAction :exec
UPDATE mandate.actions
SET description = @description, risk = @risk, wait_limit = make_interval(secs => @wait_seconds::bigint), deliver_within = make_interval(secs => @deliver_seconds::bigint), may_execute = @may_execute, schema = sqlc.narg('schema'),
    connector_kind = @connector_kind, connector_url = sqlc.narg('connector_url'), department = sqlc.narg('department'),
    removed_at = NULL
WHERE id = @id;

-- name: UpdateActionWithSecret :exec
UPDATE mandate.actions
SET description = @description, risk = @risk, wait_limit = make_interval(secs => @wait_seconds::bigint), deliver_within = make_interval(secs => @deliver_seconds::bigint), may_execute = @may_execute, schema = sqlc.narg('schema'),
    connector_kind = @connector_kind, connector_url = sqlc.narg('connector_url'), connector_secret = sqlc.narg('connector_secret'),
    department = sqlc.narg('department'), removed_at = NULL
WHERE id = @id;

-- name: RemoveAction :execrows
UPDATE mandate.actions SET removed_at = now() WHERE key = @key AND removed_at IS NULL;

-- name: GetActionByKey :one
SELECT id, tenant_id, key, description, risk,
    extract(epoch FROM wait_limit)::bigint AS wait_seconds, extract(epoch FROM deliver_within)::bigint AS deliver_seconds, may_execute, schema, connector_kind, connector_url,
    department, created_by, created_at, updated_by, updated_at, removed_at
FROM mandate.actions WHERE key = @key;

-- name: ListActions :many
SELECT id, tenant_id, key, description, risk,
    extract(epoch FROM wait_limit)::bigint AS wait_seconds, extract(epoch FROM deliver_within)::bigint AS deliver_seconds, may_execute, schema, connector_kind, connector_url,
    department, created_by, created_at, updated_by, updated_at, removed_at
FROM mandate.actions
WHERE @include_removed::boolean OR removed_at IS NULL
ORDER BY key;
