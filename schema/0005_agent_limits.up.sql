-- Limits per agent on how many proposals it may make, per minute and per
-- hour, held here so they hold across server instances: an agent that runs
-- away is stopped before it fills the inbox or spends a month's budget in
-- seconds. An owner sets them; new agents get 30 a minute and 300 an hour.

ALTER TABLE mandate.agents
    ADD COLUMN max_per_minute int NOT NULL DEFAULT 30 CHECK (max_per_minute BETWEEN 1 AND 10000),
    ADD COLUMN max_per_hour   int NOT NULL DEFAULT 300 CHECK (max_per_hour BETWEEN 1 AND 100000),
    ADD CHECK (max_per_hour >= max_per_minute);

GRANT UPDATE (max_per_minute, max_per_hour) ON mandate.agents TO mandate_app;

-- As in 0001, and an owner may change a live agent's limits.
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
    IF OLD.revoked_at IS NULL AND NEW.revoked_at IS NULL
        AND (to_jsonb(NEW) - 'max_per_minute' - 'max_per_hour') = (to_jsonb(OLD) - 'max_per_minute' - 'max_per_hour') THEN
        RETURN NEW;
    END IF;
    IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL
        OR (to_jsonb(NEW) - 'revoked_at' - 'revoked_by') <> (to_jsonb(OLD) - 'revoked_at' - 'revoked_by') THEN
        RAISE EXCEPTION 'an agent can only be revoked, once, or have its limits changed while live' USING ERRCODE = 'insufficient_privilege';
    END IF;
    NEW.revoked_by := mandate.current_actor();
    RETURN NEW;
END
$$;

-- A proposal beyond the agent's limits is refused with SQLSTATE MD429, a
-- code of pgmandate's own. An agent's proposals are counted one at a time,
-- so two at once cannot both take the last place. A repeat of a request
-- inserts nothing and is not counted, even one that waited on the lock
-- while its first copy took the last place.
CREATE FUNCTION mandate.proposals_rate_limit() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_per_minute int;
    v_per_hour   int;
    v_minute     int;
    v_hour       int;
    v_since_hour timestamptz;
    v_since_min  timestamptz;
BEGIN
    -- The count after the lock sees what committed before it only under
    -- read committed, where each statement takes a new snapshot.
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'a proposal is stored under read committed isolation only, so its agent''s limits are counted after the lock'
            USING ERRCODE = 'feature_not_supported';
    END IF;
    SELECT a.max_per_minute, a.max_per_hour INTO v_per_minute, v_per_hour
        FROM mandate.agents a WHERE a.id = NEW.agent_id AND a.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'only a known agent of the tenant proposes' USING ERRCODE = 'insufficient_privilege';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('mandate.proposals_rate_limit:' || NEW.agent_id::text, 0));
    IF EXISTS (SELECT 1 FROM mandate.proposals p
            WHERE p.tenant_id = NEW.tenant_id AND p.agent_id = NEW.agent_id AND p.idempotency_key = NEW.idempotency_key) THEN
        RETURN NEW;
    END IF;
    -- The bounds are values, not clock_timestamp() in the query, so the
    -- index on (tenant, agent, created_at) bounds the scan to the hour.
    v_since_hour := clock_timestamp() - interval '1 hour';
    v_since_min := clock_timestamp() - interval '1 minute';
    SELECT count(*) FILTER (WHERE p.created_at > v_since_min), count(*)
        INTO v_minute, v_hour
        FROM mandate.proposals p
        WHERE p.tenant_id = NEW.tenant_id AND p.agent_id = NEW.agent_id AND p.created_at > v_since_hour;
    -- The hint is the seconds until the oldest proposal in the window
    -- leaves it, when a place opens again.
    IF v_minute >= v_per_minute THEN
        RAISE EXCEPTION 'this agent may make % proposals a minute and has made %; wait a minute', v_per_minute, v_minute USING ERRCODE = 'MD429',
            HINT = (SELECT ceil(extract(epoch FROM min(p.created_at) + interval '1 minute' - clock_timestamp()))::int::text
                FROM mandate.proposals p WHERE p.tenant_id = NEW.tenant_id AND p.agent_id = NEW.agent_id AND p.created_at > v_since_min);
    END IF;
    IF v_hour >= v_per_hour THEN
        RAISE EXCEPTION 'this agent may make % proposals an hour and has made %; wait, or ask an owner to raise the limit', v_per_hour, v_hour USING ERRCODE = 'MD429',
            HINT = (SELECT ceil(extract(epoch FROM min(p.created_at) + interval '1 hour' - clock_timestamp()))::int::text
                FROM mandate.proposals p WHERE p.tenant_id = NEW.tenant_id AND p.agent_id = NEW.agent_id AND p.created_at > v_since_hour);
    END IF;
    RETURN NEW;
END
$$;

-- Named to run after proposals_insert, which sets the agent from the session.
CREATE TRIGGER proposals_rate_limit BEFORE INSERT ON mandate.proposals
    FOR EACH ROW EXECUTE FUNCTION mandate.proposals_rate_limit();
