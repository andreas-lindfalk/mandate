-- A notifier tells a tenant's people in a chat tool that a proposal waits
-- for them: a Slack or Discord incoming webhook, one per tenant. Its address
-- is a secret (anyone holding it can post to the channel), so it is kept
-- like a connector secret: written by an owner, read only by the system,
-- never returned.

CREATE TABLE mandate.notifiers (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('slack', 'discord')),
    -- Only the chat tools' own webhook addresses: the server never posts
    -- anywhere an owner points it.
    url         text NOT NULL CHECK (length(url) <= 500 AND (
                    (kind = 'slack' AND url ~ '^https://hooks\.slack\.com/services/[A-Za-z0-9/_-]+$')
                    OR (kind = 'discord' AND url ~ '^https://(discord|discordapp)\.com/api/webhooks/[0-9]+/[A-Za-z0-9_-]+$'))),
    created_by  uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    removed_at  timestamptz,
    removed_by  uuid
);

CREATE UNIQUE INDEX notifiers_live_idx ON mandate.notifiers (tenant_id) WHERE removed_at IS NULL;

ALTER TABLE mandate.notifiers ENABLE ROW LEVEL SECURITY;
CREATE POLICY notifiers_scope ON mandate.notifiers
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT INSERT ON mandate.notifiers TO mandate_app;
GRANT SELECT (id, tenant_id, kind, created_by, created_at, removed_at, removed_by) ON mandate.notifiers TO mandate_app;
GRANT UPDATE (removed_at, removed_by) ON mandate.notifiers TO mandate_app;

CREATE FUNCTION mandate.notifiers_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF mandate.current_role() IS DISTINCT FROM 'owner' THEN
        RAISE EXCEPTION 'only an owner sets where notifications go' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.created_by := mandate.current_actor();
        NEW.created_at := now();
        NEW.removed_at := NULL;
        NEW.removed_by := NULL;
        RETURN NEW;
    END IF;
    IF OLD.removed_at IS NOT NULL OR NEW.removed_at IS NULL
        OR (to_jsonb(NEW) - 'removed_at' - 'removed_by') <> (to_jsonb(OLD) - 'removed_at' - 'removed_by') THEN
        RAISE EXCEPTION 'a notifier can only be removed, once' USING ERRCODE = 'insufficient_privilege';
    END IF;
    NEW.removed_by := mandate.current_actor();
    RETURN NEW;
END
$$;

CREATE TRIGGER notifiers_guard BEFORE INSERT OR UPDATE ON mandate.notifiers
    FOR EACH ROW EXECUTE FUNCTION mandate.notifiers_guard();
CREATE TRIGGER notifiers_no_delete BEFORE DELETE ON mandate.notifiers
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

-- notifier_of gives the system, and only the system, the tenant's live
-- notifier with its address.
CREATE FUNCTION mandate.notifier_of() RETURNS TABLE (kind text, url text)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = mandate, pg_temp AS $$
BEGIN
    IF mandate.current_actor_kind() IS DISTINCT FROM 'system' OR mandate.current_tenant() IS NULL THEN
        RAISE EXCEPTION 'only the system reads where notifications go' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN QUERY SELECT n.kind, n.url FROM mandate.notifiers n
        WHERE n.tenant_id = mandate.current_tenant() AND n.removed_at IS NULL;
END
$$;
REVOKE ALL ON FUNCTION mandate.notifier_of() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mandate.notifier_of() TO mandate_app;
