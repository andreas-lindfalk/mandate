-- Back to 0004: no limits per agent.

DROP TRIGGER proposals_rate_limit ON mandate.proposals;
DROP FUNCTION mandate.proposals_rate_limit();

CREATE OR REPLACE FUNCTION mandate.agents_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF mandate.current_role() IS DISTINCT FROM 'owner' THEN
        RAISE EXCEPTION 'only an owner registers or revokes agents' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.created_by := mandate.current_actor();
        NEW.created_at := now();
        NEW.revoked_at := NULL;
        NEW.revoked_by := NULL;
        RETURN NEW;
    END IF;
    IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL
        OR (to_jsonb(NEW) - 'revoked_at' - 'revoked_by') <> (to_jsonb(OLD) - 'revoked_at' - 'revoked_by') THEN
        RAISE EXCEPTION 'an agent can only be revoked, once' USING ERRCODE = 'insufficient_privilege';
    END IF;
    NEW.revoked_by := mandate.current_actor();
    RETURN NEW;
END
$$;

ALTER TABLE mandate.agents DROP COLUMN max_per_minute, DROP COLUMN max_per_hour;
