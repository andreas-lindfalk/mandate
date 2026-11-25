-- name: InsertTenant :exec
INSERT INTO mandate.tenants (id, name) VALUES (@id, @name);

-- name: GetTenant :one
SELECT * FROM mandate.tenants WHERE id = @id;

-- name: InsertPerson :exec
INSERT INTO mandate.persons (id, tenant_id, email, name, role) VALUES (@id, @tenant_id, @email, @name, @role);

-- name: GetPerson :one
SELECT * FROM mandate.persons WHERE id = @id;

-- name: FindPerson :one
SELECT * FROM mandate.persons WHERE email = @email AND removed_at IS NULL;

-- name: RemovePerson :execrows
UPDATE mandate.persons SET removed_at = now() WHERE id = @id AND removed_at IS NULL;

-- name: ListPersons :many
SELECT * FROM mandate.persons ORDER BY created_at, id;
