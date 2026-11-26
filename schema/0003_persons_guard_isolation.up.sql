-- 0001's guard counts the other owners after the tenant's lock, which
-- holds only when that count sees what committed before it. Under
-- repeatable read or serializable the snapshot is older than the lock, so
-- two owners removing each other at once could both pass and leave the
-- tenant without an owner. Like the budget and the agent's limits, a
-- change to people under another isolation level is now refused.
CREATE OR REPLACE FUNCTION mandate.persons_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- One change to a tenant's people at a time, so that "the first person"
    -- and "the last owner" are judged against what is really there: two
    -- owners removing each other at once cannot both pass.
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text, 11));
    -- That judgement after the lock sees what committed before it only
    -- under read committed, where each statement takes a new snapshot.
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'people are changed under read committed isolation only, so the last owner is counted after the lock'
            USING ERRCODE = 'feature_not_supported';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF mandate.current_role() IS DISTINCT FROM 'owner' AND NOT (
            mandate.current_actor_kind() = 'system'
            AND NOT EXISTS (SELECT 1 FROM mandate.persons p WHERE p.tenant_id = NEW.tenant_id)) THEN
            RAISE EXCEPTION 'only an owner adds people' USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.created_at := now();
        NEW.removed_at := NULL;
        NEW.removed_by := NULL;
        -- A person is bound at their own first sign-in, never on the way in.
        NEW.issuer := NULL;
        NEW.subject := NULL;
        RETURN NEW;
    END IF;
    -- The binding: by the system, once, both columns together, nothing else
    -- changed, and only for a person who is not removed.
    IF NEW.issuer IS DISTINCT FROM OLD.issuer OR NEW.subject IS DISTINCT FROM OLD.subject THEN
        IF mandate.current_actor_kind() IS DISTINCT FROM 'system' OR OLD.issuer IS NOT NULL
            OR NEW.issuer IS NULL OR OLD.removed_at IS NOT NULL
            OR (to_jsonb(NEW) - 'issuer' - 'subject') <> (to_jsonb(OLD) - 'issuer' - 'subject') THEN
            RAISE EXCEPTION 'a person is bound to an identity once, by the system, and never changed' USING ERRCODE = 'insufficient_privilege';
        END IF;
        RETURN NEW;
    END IF;
    IF mandate.current_role() IS DISTINCT FROM 'owner' THEN
        RAISE EXCEPTION 'only an owner removes people' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.removed_at IS NOT NULL OR NEW.removed_at IS NULL
        OR (to_jsonb(NEW) - 'removed_at' - 'removed_by') <> (to_jsonb(OLD) - 'removed_at' - 'removed_by') THEN
        RAISE EXCEPTION 'a person can only be removed, once' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.role = 'owner' AND NOT EXISTS (
        SELECT 1 FROM mandate.persons p
        WHERE p.tenant_id = OLD.tenant_id AND p.role = 'owner' AND p.removed_at IS NULL AND p.id <> OLD.id) THEN
        RAISE EXCEPTION 'the last owner cannot be removed' USING ERRCODE = 'check_violation';
    END IF;
    NEW.removed_by := mandate.current_actor();
    RETURN NEW;
END
$$;
