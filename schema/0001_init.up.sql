-- pgmandate: agents propose; a person, or a mandate a person granted, decides;
-- Postgres enforces the rules a second time.
--
-- Everything lives in the schema "mandate". The application connects as
-- mandate_app, which never owns tables and never bypasses row level
-- security. Every transaction sets the scope (tenant, actor kind, actor id,
-- run id) and the policies and triggers below read it.
CREATE SCHEMA mandate;
GRANT USAGE ON SCHEMA mandate TO mandate_app;

-- Scope helpers. An unset or empty setting yields NULL, which matches no rows.
CREATE FUNCTION mandate.current_tenant() RETURNS uuid
LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('mandate.tenant_id', true), '')::uuid
$$;

CREATE FUNCTION mandate.current_actor() RETURNS uuid
LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('mandate.actor_id', true), '')::uuid
$$;

CREATE FUNCTION mandate.current_actor_kind() RETURNS text
LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('mandate.actor_kind', true), '')
$$;

CREATE FUNCTION mandate.current_run() RETURNS text
LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('mandate.run_id', true), '')
$$;

-- While the system executes a proposal under a mandate, the transaction
-- names the proposal. Nothing else ever sets this.
CREATE FUNCTION mandate.executing_proposal() RETURNS uuid
LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('mandate.executing_proposal', true), '')::uuid
$$;

GRANT EXECUTE ON FUNCTION mandate.current_tenant(), mandate.current_actor(), mandate.current_actor_kind(),
    mandate.current_run(), mandate.executing_proposal() TO mandate_app;

-- Append-only guard. Rows leave only through purge_tenant, which is owner-only.
CREATE FUNCTION mandate.forbid_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE'
       AND current_setting('mandate.allow_purge', true) = 'on'
       AND current_user = (SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = TG_RELID) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION '% is append-only (% refused)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'insufficient_privilege';
END
$$;

------------------------------------------------------------------------------
-- Tenants and persons
------------------------------------------------------------------------------
CREATE TABLE mandate.tenants (
    id          uuid PRIMARY KEY,
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    created_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE mandate.tenants ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenants_scope ON mandate.tenants
    USING (id = mandate.current_tenant())
    WITH CHECK (id = mandate.current_tenant());

GRANT SELECT, INSERT ON mandate.tenants TO mandate_app;

-- Persons decide. An owner also grants mandates and registers agents; a
-- reviewer only decides proposals.
CREATE TABLE mandate.persons (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    email       text NOT NULL CHECK (email = lower(email) AND email LIKE '%_@_%' AND length(email) <= 200),
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    role        text NOT NULL CHECK (role IN ('owner', 'reviewer')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- A removed person keeps the row, so the chain still names them, and
    -- has no role from then on.
    removed_at  timestamptz,
    removed_by  uuid
);

-- One live person per email in a tenant; a removed one can be added again,
-- as a new row.
CREATE UNIQUE INDEX persons_live_email_idx ON mandate.persons (tenant_id, email) WHERE removed_at IS NULL;

ALTER TABLE mandate.persons ENABLE ROW LEVEL SECURITY;
CREATE POLICY persons_scope ON mandate.persons
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT SELECT, INSERT ON mandate.persons TO mandate_app;
GRANT UPDATE (removed_at, removed_by) ON mandate.persons TO mandate_app;

-- The role of the person acting now, or NULL when the actor is not a person
-- of the current tenant.
CREATE FUNCTION mandate.current_role() RETURNS text
LANGUAGE sql STABLE AS $$
    SELECT p.role FROM mandate.persons p
    WHERE mandate.current_actor_kind() = 'person' AND p.id = mandate.current_actor() AND p.removed_at IS NULL
        AND p.tenant_id = mandate.current_tenant()
$$;
GRANT EXECUTE ON FUNCTION mandate.current_role() TO mandate_app;

-- may_write_for says whether the actor may write a host row that belongs to
-- p_proposal: a person of the tenant always, the system only while
-- executing exactly that proposal, an agent never. A host table that takes
-- writes from proposals puts this in its policy, and then no agent and no
-- code path outside an execution can write it.
CREATE FUNCTION mandate.may_write_for(p_proposal uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT mandate.current_role() IS NOT NULL
        OR (mandate.current_actor_kind() = 'system' AND p_proposal IS NOT NULL AND p_proposal = mandate.executing_proposal())
$$;
GRANT EXECUTE ON FUNCTION mandate.may_write_for(uuid) TO mandate_app;

-- Risk levels in order, for comparisons.
CREATE FUNCTION mandate.risk_rank(p_risk text) RETURNS int
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE p_risk WHEN 'low' THEN 0 WHEN 'medium' THEN 1 WHEN 'high' THEN 2 ELSE -1 END
$$;
GRANT EXECUTE ON FUNCTION mandate.risk_rank(text) TO mandate_app;

-- The system creates the first person of a tenant, and only the first;
-- after that owners add people. An owner removes a person, once, and never
-- the last owner.
CREATE FUNCTION mandate.persons_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- One change to a tenant's people at a time, so that "the first person"
    -- and "the last owner" are judged against what is really there: two
    -- owners removing each other at once cannot both pass.
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text, 11));
    IF TG_OP = 'INSERT' THEN
        IF mandate.current_role() IS DISTINCT FROM 'owner' AND NOT (
            mandate.current_actor_kind() = 'system'
            AND NOT EXISTS (SELECT 1 FROM mandate.persons p WHERE p.tenant_id = NEW.tenant_id)) THEN
            RAISE EXCEPTION 'only an owner adds people' USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.created_at := now();
        NEW.removed_at := NULL;
        NEW.removed_by := NULL;
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

CREATE TRIGGER persons_guard BEFORE INSERT OR UPDATE ON mandate.persons
    FOR EACH ROW EXECUTE FUNCTION mandate.persons_guard();
CREATE TRIGGER persons_no_delete BEFORE DELETE ON mandate.persons
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

------------------------------------------------------------------------------
-- Agents and their tokens
------------------------------------------------------------------------------
-- An agent is a principal of its own: it proposes in its own name and the
-- chain never shows its work as a person's.
CREATE TABLE mandate.agents (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    -- An optional label: the part of the organisation the agent works for.
    department  text CHECK (department IS NULL OR length(department) BETWEEN 1 AND 100),
    created_by  uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz,
    revoked_by  uuid,
    max_per_minute int NOT NULL DEFAULT 30 CHECK (max_per_minute BETWEEN 1 AND 10000),
    max_per_hour   int NOT NULL DEFAULT 300 CHECK (max_per_hour BETWEEN 1 AND 100000),
    CHECK (max_per_hour >= max_per_minute),
    -- So that rows referring to an agent can also name its tenant, and a
    -- foreign key can never point into another tenant.
    UNIQUE (tenant_id, id)
);

CREATE INDEX agents_tenant_idx ON mandate.agents (tenant_id, created_at DESC);

ALTER TABLE mandate.agents ENABLE ROW LEVEL SECURITY;
CREATE POLICY agents_scope ON mandate.agents
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT SELECT, INSERT ON mandate.agents TO mandate_app;
GRANT UPDATE (revoked_at, revoked_by) ON mandate.agents TO mandate_app;
GRANT UPDATE (max_per_minute, max_per_hour) ON mandate.agents TO mandate_app;

CREATE FUNCTION mandate.agents_guard() RETURNS trigger
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

CREATE TRIGGER agents_guard BEFORE INSERT OR UPDATE ON mandate.agents
    FOR EACH ROW EXECUTE FUNCTION mandate.agents_guard();
CREATE TRIGGER agents_no_delete BEFORE DELETE ON mandate.agents
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

-- Only the SHA-256 hash of a token is stored. The hint is its last
-- characters, so people can tell tokens apart.
CREATE TABLE mandate.agent_tokens (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    agent_id      uuid NOT NULL,
    token_hash    bytea NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    hint          text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    last_used_at  timestamptz,
    revoked_at    timestamptz,
    CHECK (expires_at > created_at),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES mandate.agents (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX agent_tokens_agent_idx ON mandate.agent_tokens (tenant_id, agent_id);

ALTER TABLE mandate.agent_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY agent_tokens_scope ON mandate.agent_tokens
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT SELECT, INSERT ON mandate.agent_tokens TO mandate_app;
GRANT UPDATE (last_used_at, revoked_at) ON mandate.agent_tokens TO mandate_app;

CREATE FUNCTION mandate.agent_tokens_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF mandate.current_role() IS DISTINCT FROM 'owner' THEN
            RAISE EXCEPTION 'only an owner issues agent tokens' USING ERRCODE = 'insufficient_privilege';
        END IF;
        IF NEW.revoked_at IS NOT NULL OR NEW.last_used_at IS NOT NULL THEN
            RAISE EXCEPTION 'a new token is unused and not revoked' USING ERRCODE = 'check_violation';
        END IF;
    ELSIF NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        -- Only an owner revokes, and a revoked token stays revoked. The
        -- agent itself only ever touches last_used_at.
        IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL THEN
            RAISE EXCEPTION 'a revoked token stays revoked' USING ERRCODE = 'check_violation';
        END IF;
        IF mandate.current_role() IS DISTINCT FROM 'owner' THEN
            RAISE EXCEPTION 'only an owner revokes agent tokens' USING ERRCODE = 'insufficient_privilege';
        END IF;
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER agent_tokens_guard BEFORE INSERT OR UPDATE ON mandate.agent_tokens
    FOR EACH ROW EXECUTE FUNCTION mandate.agent_tokens_guard();
CREATE TRIGGER agent_tokens_no_delete BEFORE DELETE ON mandate.agent_tokens
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

-- The tenant is not known until the token is, so the lookup runs before row
-- level security can be scoped. It returns only live tokens of live agents.
CREATE FUNCTION mandate.resolve_agent_token(p_hash bytea)
RETURNS TABLE (id uuid, tenant_id uuid, agent_id uuid, expires_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = mandate, pg_temp AS $$
    SELECT t.id, t.tenant_id, t.agent_id, t.expires_at
    FROM mandate.agent_tokens t
    JOIN mandate.agents a ON a.id = t.agent_id
    WHERE t.token_hash = p_hash AND t.revoked_at IS NULL AND t.expires_at > now() AND a.revoked_at IS NULL
$$;
REVOKE ALL ON FUNCTION mandate.resolve_agent_token(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mandate.resolve_agent_token(bytea) TO mandate_app;

------------------------------------------------------------------------------
-- Mandates
------------------------------------------------------------------------------
-- An action key is dotted lower case: tickets.create. A pattern in a
-- mandate is a key, a prefix with .* (tickets.*), or * for everything.
CREATE FUNCTION mandate.valid_action_key(p_key text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT p_key ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$' AND length(p_key) <= 100
$$;

CREATE FUNCTION mandate.valid_action_patterns(p_patterns text[]) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT cardinality(p_patterns) BETWEEN 1 AND 50 AND NOT EXISTS (
        SELECT 1 FROM unnest(p_patterns) AS p
        WHERE NOT (p = '*' OR mandate.valid_action_key(p) OR (right(p, 2) = '.*' AND mandate.valid_action_key(left(p, -2))))
    )
$$;

-- covers says whether a mandate's patterns admit an action.
CREATE FUNCTION mandate.covers(p_patterns text[], p_action text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM unnest(p_patterns) AS p
        WHERE p = '*' OR p = p_action OR (right(p, 2) = '.*' AND left(p_action, length(p) - 1) = left(p, -1))
    )
$$;

GRANT EXECUTE ON FUNCTION mandate.valid_action_key(text), mandate.valid_action_patterns(text[]), mandate.covers(text[], text) TO mandate_app;

-- Conditions on a mandate: a CEL expression over the payload and the
-- action that narrows what the mandate allows. The service compiles it at
-- the grant to a plan, stored beside it; the service and this database
-- evaluate the same plan, and an execution under the mandate holds only
-- when the database finds the plan true for the payload executed.

-- A plan is held to the shape the service compiles: the known ops, paths
-- into the payload or the action, literals that are numbers, strings or
-- booleans, at most 64 levels and 20 kB. The plan cannot be proved to be
-- the compilation of the text beside it; that is the service's to keep.
CREATE FUNCTION mandate.valid_condition_path(p_path jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT jsonb_typeof(p_path) = 'array'
        AND jsonb_array_length(p_path) BETWEEN 1 AND 11
        AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(p_path) AS s
            WHERE jsonb_typeof(s) IS DISTINCT FROM 'string' OR octet_length(s #>> '{}') NOT BETWEEN 1 AND 200)
        AND ((p_path->>0 = 'payload' AND jsonb_array_length(p_path) >= 2)
            OR (p_path->>0 = 'action' AND jsonb_array_length(p_path) = 1))
$$;

CREATE FUNCTION mandate.valid_condition_node(p_node jsonb, p_depth int) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    v_op    text := p_node->>'op';
    v_arg   jsonb;
    v_kind  text;
BEGIN
    IF p_depth > 64 OR jsonb_typeof(p_node) IS DISTINCT FROM 'object'
        OR EXISTS (SELECT 1 FROM jsonb_object_keys(p_node) AS k WHERE k NOT IN ('op', 'args', 'path', 'value', 'values')) THEN
        RETURN false;
    END IF;
    IF v_op IN ('and', 'or', 'not') THEN
        IF jsonb_typeof(p_node->'args') IS DISTINCT FROM 'array'
            OR jsonb_array_length(p_node->'args') NOT BETWEEN 1 AND 100
            OR (v_op = 'not' AND jsonb_array_length(p_node->'args') <> 1) THEN
            RETURN false;
        END IF;
        FOR v_arg IN SELECT a FROM jsonb_array_elements(p_node->'args') AS a LOOP
            IF NOT mandate.valid_condition_node(v_arg, p_depth + 1) THEN
                RETURN false;
            END IF;
        END LOOP;
        RETURN true;
    ELSIF v_op IN ('==', '!=', '<', '<=', '>', '>=') THEN
        v_kind := jsonb_typeof(p_node->'value');
        RETURN mandate.valid_condition_path(p_node->'path')
            AND v_kind IN ('number', 'string', 'boolean')
            AND (v_kind <> 'boolean' OR v_op IN ('==', '!='))
            AND (p_node->'path'->>0 <> 'action' OR (v_kind = 'string' AND v_op IN ('==', '!=')));
    ELSIF v_op = 'in' THEN
        IF NOT mandate.valid_condition_path(p_node->'path') OR jsonb_typeof(p_node->'values') IS DISTINCT FROM 'array'
            OR jsonb_array_length(p_node->'values') NOT BETWEEN 1 AND 100 THEN
            RETURN false;
        END IF;
        v_kind := jsonb_typeof(p_node->'values'->0);
        RETURN v_kind IN ('number', 'string', 'boolean')
            AND (p_node->'path'->>0 <> 'action' OR v_kind = 'string')
            AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(p_node->'values') AS v WHERE jsonb_typeof(v) <> v_kind);
    ELSIF v_op = 'has' THEN
        RETURN mandate.valid_condition_path(p_node->'path') AND p_node->'path'->>0 = 'payload';
    END IF;
    RETURN false;
END
$$;

CREATE FUNCTION mandate.valid_condition_plan(p_plan jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT octet_length(p_plan::text) <= 20000 AND mandate.valid_condition_node(p_plan, 1)
$$;

-- A mandate is the policy that allows an agent to act without a person:
-- which actions, up to which risk, within which budget per calendar month,
-- under which adopted policy document, until when. Granted by an owner,
-- never changed, revoked once. High risk is never delegated.
CREATE TABLE mandate.mandates (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    -- NULL means every agent of the tenant.
    agent_id      uuid,
    actions       text[] NOT NULL CHECK (mandate.valid_action_patterns(actions)),
    max_risk      text NOT NULL CHECK (max_risk IN ('low', 'medium')),
    -- How many executions it may allow per UTC calendar month.
    budget_count  bigint NOT NULL CHECK (budget_count BETWEEN 1 AND 1000000000),
    policy_ref    text NOT NULL CHECK (length(policy_ref) BETWEEN 1 AND 200),
    department    text CHECK (department IS NULL OR length(department) BETWEEN 1 AND 100),
    granted_by    uuid,
    valid_from    timestamptz NOT NULL DEFAULT now(),
    valid_until   timestamptz NOT NULL,
    revoked_at    timestamptz,
    revoked_by    uuid,
    condition      text CHECK (condition IS NULL OR length(condition) BETWEEN 1 AND 2000),
    condition_plan jsonb CHECK (condition_plan IS NULL OR mandate.valid_condition_plan(condition_plan)),
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- The difference of two instants is exact, whatever the session's time zone.
    CHECK ((condition IS NULL) = (condition_plan IS NULL)),
    CHECK (valid_until > valid_from AND valid_until - valid_from <= interval '366 days'),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES mandate.agents (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX mandates_tenant_idx ON mandate.mandates (tenant_id, valid_until DESC);

ALTER TABLE mandate.mandates ENABLE ROW LEVEL SECURITY;
CREATE POLICY mandates_scope ON mandate.mandates
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT SELECT, INSERT ON mandate.mandates TO mandate_app;
GRANT UPDATE (revoked_at, revoked_by) ON mandate.mandates TO mandate_app;

CREATE FUNCTION mandate.mandates_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF mandate.current_role() IS DISTINCT FROM 'owner' THEN
        RAISE EXCEPTION 'only an owner grants or revokes a mandate' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.granted_by := mandate.current_actor();
        NEW.created_at := now();
        NEW.revoked_at := NULL;
        NEW.revoked_by := NULL;
        RETURN NEW;
    END IF;
    IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL
        OR (to_jsonb(NEW) - 'revoked_at' - 'revoked_by') <> (to_jsonb(OLD) - 'revoked_at' - 'revoked_by') THEN
        RAISE EXCEPTION 'a mandate can only be revoked, once' USING ERRCODE = 'insufficient_privilege';
    END IF;
    NEW.revoked_by := mandate.current_actor();
    RETURN NEW;
END
$$;

CREATE TRIGGER mandates_guard BEFORE INSERT OR UPDATE ON mandate.mandates
    FOR EACH ROW EXECUTE FUNCTION mandate.mandates_guard();
CREATE TRIGGER mandates_no_delete BEFORE DELETE ON mandate.mandates
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

------------------------------------------------------------------------------
-- Actions
------------------------------------------------------------------------------
-- What an agent may propose, as an owner defines it: a key, a payload
-- schema, a default risk, how long its proposals may wait and its decisions
-- be carried out, whether a mandate may ever let it run
-- without a person, and the connector that carries it out: a signed
-- webhook, or builtin code in an embedded host. A removed action keeps its
-- row, so old proposals still read.
CREATE TABLE mandate.actions (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    key               text NOT NULL CHECK (mandate.valid_action_key(key)),
    description       text NOT NULL CHECK (length(description) <= 1000),
    risk              text NOT NULL CHECK (risk IN ('low', 'medium', 'high')),
    -- How long a proposal for it may wait for a decision, and how long after a
    -- decision its delivery may still be carried out. Fixed on each proposal.
    wait_limit        interval NOT NULL DEFAULT '168 hours' CHECK (wait_limit BETWEEN interval '1 minute' AND interval '30 days'),
    deliver_within    interval NOT NULL DEFAULT '1 hour' CHECK (deliver_within BETWEEN interval '2 minutes' AND interval '7 days'),
    may_execute       boolean NOT NULL DEFAULT false,
    schema            jsonb CHECK (schema IS NULL OR jsonb_typeof(schema) = 'object'),
    connector_kind    text NOT NULL CHECK (connector_kind IN ('webhook', 'builtin')),
    connector_url     text CHECK (connector_url IS NULL OR (length(connector_url) <= 2000 AND connector_url ~ '^https?://')),
    -- Who a delivery is for: pgm:<tenant>:<key>:<16 random hex digits>. The
    -- trigger draws it, again whenever the address changes or the action is
    -- defined anew, so an audience never repeats. A receiver accepts only its
    -- own, and never takes it from the request.
    connector_audience text CHECK (connector_audience IS NULL OR connector_audience ~ '^pgm:[0-9a-f-]{36}:[a-z0-9._-]+:[0-9a-f]{16}$'),
    department        text CHECK (department IS NULL OR length(department) BETWEEN 1 AND 100),
    created_by        uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_by        uuid,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    removed_at        timestamptz,
    UNIQUE (tenant_id, key),
    -- A webhook has an address and an audience; a builtin action has neither.
    CHECK ((connector_kind = 'webhook') = (connector_url IS NOT NULL)),
    CHECK ((connector_kind = 'webhook') = (connector_audience IS NOT NULL)),
    -- What is delivered to another system is held to a schema.
    CHECK (connector_kind <> 'webhook' OR schema IS NOT NULL)
);

ALTER TABLE mandate.actions ENABLE ROW LEVEL SECURITY;
CREATE POLICY actions_scope ON mandate.actions
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT SELECT, INSERT, UPDATE ON mandate.actions TO mandate_app;

CREATE FUNCTION mandate.actions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF mandate.current_role() IS DISTINCT FROM 'owner' THEN
        RAISE EXCEPTION 'only an owner defines actions' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.created_by := mandate.current_actor();
        NEW.created_at := now();
        NEW.removed_at := NULL;
    ELSIF NEW.tenant_id <> OLD.tenant_id OR NEW.key <> OLD.key OR NEW.id <> OLD.id
        OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'an action keeps its key' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- The audience is the database's to draw, never the caller's. A new one
    -- for a new webhook, a webhook moved to a new address, or an action
    -- defined again after removal: the receiver at the old address, or of
    -- the old definition, then accepts nothing meant for the new.
    IF NEW.connector_kind <> 'webhook' THEN
        NEW.connector_audience := NULL;
    ELSIF TG_OP = 'INSERT' OR OLD.connector_kind <> 'webhook'
        OR NEW.connector_url IS DISTINCT FROM OLD.connector_url
        OR (OLD.removed_at IS NOT NULL AND NEW.removed_at IS NULL) THEN
        NEW.connector_audience := 'pgm:' || NEW.tenant_id || ':' || NEW.key || ':' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 16);
    ELSE
        NEW.connector_audience := OLD.connector_audience;
    END IF;
    NEW.updated_by := mandate.current_actor();
    NEW.updated_at := now();
    RETURN NEW;
END
$$;


CREATE TRIGGER actions_guard BEFORE INSERT OR UPDATE ON mandate.actions
    FOR EACH ROW EXECUTE FUNCTION mandate.actions_guard();
CREATE TRIGGER actions_no_delete BEFORE DELETE ON mandate.actions
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

-- The agent's account of a proposal: why it proposes it (rationale), what
-- it looked at (evidence), and who wrote it (provenance: the MCP client as
-- it introduced itself, the model and the agent's own version, as the
-- agent states them). The person deciding reads it; it is kept as written
-- and never changed. The log holds its digests.

-- Evidence is a list of at most 20 items, each where the agent looked
-- (source), an id a person can follow (ref, optional), and what it showed
-- (summary).
CREATE FUNCTION mandate.valid_evidence(p_evidence jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT jsonb_typeof(p_evidence) = 'array' AND jsonb_array_length(p_evidence) <= 20
        AND NOT EXISTS (
            SELECT 1 FROM jsonb_array_elements(p_evidence) AS e
            WHERE jsonb_typeof(e) IS DISTINCT FROM 'object'
                OR EXISTS (SELECT 1 FROM jsonb_object_keys(e) AS k WHERE k NOT IN ('source', 'ref', 'summary'))
                OR jsonb_typeof(e->'source') IS DISTINCT FROM 'string' OR length(e->>'source') NOT BETWEEN 1 AND 100
                OR jsonb_typeof(e->'summary') IS DISTINCT FROM 'string' OR length(e->>'summary') NOT BETWEEN 1 AND 1000
                OR (e ? 'ref' AND (jsonb_typeof(e->'ref') IS DISTINCT FROM 'string' OR length(e->>'ref') NOT BETWEEN 1 AND 500)))
$$;

-- Provenance names, never prose: short labels in a narrow alphabet without
-- spaces, since the log keeps them as written.
CREATE FUNCTION mandate.valid_provenance(p_provenance jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT jsonb_typeof(p_provenance) = 'object'
        AND NOT EXISTS (
            SELECT 1 FROM jsonb_each(p_provenance) AS f
            WHERE f.key NOT IN ('client', 'clientVersion', 'model', 'agentVersion')
                OR jsonb_typeof(f.value) IS DISTINCT FROM 'string'
                OR (f.value #>> '{}') !~ '^[A-Za-z0-9][A-Za-z0-9._:/@+()-]{0,99}$')
$$;

GRANT EXECUTE ON FUNCTION mandate.valid_evidence(jsonb), mandate.valid_provenance(jsonb) TO mandate_app;

-- Proposals made before this migration have no rationale: NULL says so.
------------------------------------------------------------------------------
-- Proposals
------------------------------------------------------------------------------
-- The one way an agent changes anything. An agent inserts; a person
-- approves, edits or rejects; the system executes under a mandate. The
-- triggers enforce all of it, whatever the application code does.
CREATE TABLE mandate.proposals (
    id               uuid PRIMARY KEY,
    tenant_id        uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    agent_id         uuid NOT NULL,
    run_id           text CHECK (run_id IS NULL OR run_id ~ '^[A-Za-z0-9._:-]{1,100}$'),
    action           text NOT NULL CHECK (mandate.valid_action_key(action)),
    risk             text NOT NULL CHECK (risk IN ('low', 'medium', 'high')),
    title            text NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    payload          jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    -- Until when it may wait for a decision, and how long after the decision
    -- its delivery may be carried out. Set by the insert trigger from the
    -- action, so a later change to the action changes nothing here.
    expires_at       timestamptz NOT NULL,
    deliver_within   interval NOT NULL,
    -- Flags are what mandate itself noticed, never what the agent said.
    flags            text[] NOT NULL DEFAULT '{}',
    -- The agent's name for this one request. Every proposal has one, and
    -- it is unique per agent: a repeat finds the first proposal instead of
    -- making a second.
    idempotency_key  text NOT NULL CHECK (idempotency_key ~ '^[\x21-\x7E]{1,200}$'),
    -- What was asked for under the key: a hash of the action and the
    -- payload as proposed. A person's edit changes the payload, never this,
    -- so a repeat is still recognised after an edit.
    request_hash     bytea NOT NULL CHECK (length(request_hash) = 32),
    -- The payload as the agent proposed it, set by the database and never
    -- changed: a person's edit changes payload, and the two together show
    -- what was edited.
    proposed_payload jsonb NOT NULL,
    -- The agent's account: why it proposes this (rationale), what it looked
    -- at (evidence), and who wrote it (provenance). Kept as written.
    rationale        text CHECK (rationale IS NULL OR length(rationale) BETWEEN 1 AND 8000),
    evidence         jsonb NOT NULL DEFAULT '[]' CHECK (mandate.valid_evidence(evidence)),
    provenance       jsonb NOT NULL DEFAULT '{}' CHECK (mandate.valid_provenance(provenance)),
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'executed', 'withdrawn', 'expired')),
    edited           boolean NOT NULL DEFAULT false,
    decided_by       uuid,
    decided_at       timestamptz,
    decision_note    text CHECK (decision_note IS NULL OR length(decision_note) <= 1000),
    execution_basis  jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, agent_id, idempotency_key),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, agent_id) REFERENCES mandate.agents (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX proposals_status_idx ON mandate.proposals (tenant_id, status, created_at DESC);
CREATE INDEX proposals_action_idx ON mandate.proposals (tenant_id, action);
CREATE INDEX proposals_agent_idx ON mandate.proposals (tenant_id, agent_id, created_at DESC);
CREATE INDEX proposals_executed_idx ON mandate.proposals (tenant_id, ((execution_basis->>'mandateId')), decided_at)
    WHERE status = 'executed';
-- For the sweep: waiting proposals by deadline.
CREATE INDEX proposals_expiry_idx ON mandate.proposals (expires_at) WHERE status = 'pending';

ALTER TABLE mandate.proposals ENABLE ROW LEVEL SECURITY;
CREATE POLICY proposals_scope ON mandate.proposals
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT SELECT, INSERT, UPDATE ON mandate.proposals TO mandate_app;

-- The proposer is always the agent in scope, never what the caller wrote.
CREATE FUNCTION mandate.proposals_on_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_removed_at     timestamptz;
    v_kind           text;
    v_risk           text;
    v_wait_limit     interval;
    v_deliver_within interval;
BEGIN
    IF mandate.current_actor_kind() IS DISTINCT FROM 'agent' OR mandate.current_actor() IS NULL THEN
        RAISE EXCEPTION 'only an agent proposes' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM mandate.agents a
            WHERE a.id = mandate.current_actor() AND a.tenant_id = NEW.tenant_id AND a.revoked_at IS NULL) THEN
        RAISE EXCEPTION 'only a live agent of the tenant proposes' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- Where the tenant has defined the action, the proposal is held to the
    -- definition: not removed, and for a webhook action a risk no lower than
    -- the definition's. An action that exists only as code in an embedded
    -- host has no row to check against; there the host's code is the
    -- authority, and the waiting time and delivery window are the defaults.
    SELECT a.removed_at, a.connector_kind, a.risk, a.wait_limit, a.deliver_within
        INTO v_removed_at, v_kind, v_risk, v_wait_limit, v_deliver_within
        FROM mandate.actions a WHERE a.tenant_id = NEW.tenant_id AND a.key = NEW.action;
    IF FOUND THEN
        IF v_removed_at IS NOT NULL THEN
            RAISE EXCEPTION 'the action % is removed', NEW.action USING ERRCODE = 'insufficient_privilege';
        END IF;
        IF v_kind = 'webhook' AND mandate.risk_rank(NEW.risk) < mandate.risk_rank(v_risk) THEN
            RAISE EXCEPTION 'a proposal cannot claim a lower risk than its action' USING ERRCODE = 'insufficient_privilege';
        END IF;
    END IF;
    NEW.agent_id := mandate.current_actor();
    NEW.run_id := mandate.current_run();
    NEW.status := 'pending';
    NEW.edited := false;
    NEW.decided_by := NULL;
    NEW.decided_at := NULL;
    NEW.decision_note := NULL;
    NEW.execution_basis := NULL;
    NEW.proposed_payload := NEW.payload;
    NEW.created_at := now();
    -- Hours, not days: a day in an interval follows the session's time zone
    -- across a daylight saving change, and the defaults are exact.
    NEW.expires_at := now() + coalesce(v_wait_limit, interval '168 hours');
    NEW.deliver_within := coalesce(v_deliver_within, interval '1 hour');
    RETURN NEW;
END
$$;

-- How many executions a mandate has allowed in the current UTC calendar
-- month. The budget period is the database's clock.
CREATE FUNCTION mandate.spent_count(p_mandate uuid) RETURNS bigint
LANGUAGE sql STABLE AS $$
    SELECT count(*)::bigint FROM mandate.proposals p
    WHERE p.tenant_id = (SELECT m.tenant_id FROM mandate.mandates m WHERE m.id = p_mandate)
        AND p.status = 'executed' AND p.execution_basis->>'mandateId' = p_mandate::text
        AND p.decided_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
        AND p.decided_at < (date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC'
$$;
GRANT EXECUTE ON FUNCTION mandate.spent_count(uuid) TO mandate_app;

-- The value a plan's path names: ["action"], or ["payload", key, ...] where
-- every step is an object, so a key is never read as an array index. NULL
-- when there is no such value.
CREATE FUNCTION mandate.condition_value(p_path jsonb, p_action text, p_payload jsonb) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    v_current jsonb := p_payload;
BEGIN
    IF jsonb_typeof(p_path) IS DISTINCT FROM 'array' OR jsonb_array_length(p_path) = 0
        OR jsonb_typeof(p_path->0) IS DISTINCT FROM 'string' THEN
        RETURN NULL;
    END IF;
    IF p_path->>0 = 'action' THEN
        RETURN CASE WHEN jsonb_array_length(p_path) = 1 THEN to_jsonb(p_action) END;
    END IF;
    IF p_path->>0 <> 'payload' OR jsonb_array_length(p_path) < 2 THEN
        RETURN NULL;
    END IF;
    FOR i IN 1 .. jsonb_array_length(p_path) - 1 LOOP
        IF jsonb_typeof(v_current) IS DISTINCT FROM 'object' OR jsonb_typeof(p_path->i) IS DISTINCT FROM 'string' THEN
            RETURN NULL;
        END IF;
        v_current := v_current -> (p_path->>i);
    END LOOP;
    RETURN v_current;
END
$$;

-- A comparison of two JSON values of the same type: numbers exactly as
-- numeric, strings by their bytes (collation "C"), booleans by == and !=
-- only. NULL when the types differ or the operator does not apply.
CREATE FUNCTION mandate.condition_compare(p_op text, p_left jsonb, p_right jsonb) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    v_type text := jsonb_typeof(p_left);
    v_cmp  int;
BEGIN
    IF p_left IS NULL OR p_right IS NULL OR v_type IS DISTINCT FROM jsonb_typeof(p_right) THEN
        RETURN NULL;
    END IF;
    IF v_type = 'number' THEN
        v_cmp := CASE WHEN p_left::numeric < p_right::numeric THEN -1 WHEN p_left::numeric = p_right::numeric THEN 0 ELSE 1 END;
    ELSIF v_type = 'string' THEN
        v_cmp := CASE
            WHEN (p_left #>> '{}') COLLATE "C" < (p_right #>> '{}') COLLATE "C" THEN -1
            WHEN (p_left #>> '{}') COLLATE "C" = (p_right #>> '{}') COLLATE "C" THEN 0
            ELSE 1 END;
    ELSIF v_type = 'boolean' AND p_op IN ('==', '!=') THEN
        v_cmp := CASE WHEN p_left = p_right THEN 0 ELSE 1 END;
    ELSE
        RETURN NULL;
    END IF;
    RETURN CASE p_op
        WHEN '==' THEN v_cmp = 0
        WHEN '!=' THEN v_cmp <> 0
        WHEN '<' THEN v_cmp < 0
        WHEN '<=' THEN v_cmp <= 0
        WHEN '>' THEN v_cmp > 0
        WHEN '>=' THEN v_cmp >= 0
    END;
END
$$;

-- A condition's plan, evaluated on an action and a payload: true, false, or
-- NULL when it cannot be evaluated (a missing field, a type that does not
-- match, a plan it does not read). Only true allows. The service's
-- evaluation (mandates.Plan) is the same, case for case, and a test runs
-- both on the same cases.
CREATE FUNCTION mandate.condition_holds(p_plan jsonb, p_action text, p_payload jsonb) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    v_op      text := p_plan->>'op';
    v_arg     jsonb;
    v_result  boolean;
    v_unknown boolean := false;
    v_value   jsonb;
    v_parent  jsonb;
    v_length  int;
BEGIN
    IF jsonb_typeof(p_plan) IS DISTINCT FROM 'object' THEN
        RETURN NULL;
    END IF;
    IF v_op IN ('and', 'or') THEN
        IF jsonb_typeof(p_plan->'args') IS DISTINCT FROM 'array' OR jsonb_array_length(p_plan->'args') = 0 THEN
            RETURN NULL;
        END IF;
        -- false decides and, true decides or, whatever else is unknown.
        FOR v_arg IN SELECT a FROM jsonb_array_elements(p_plan->'args') AS a LOOP
            v_result := mandate.condition_holds(v_arg, p_action, p_payload);
            IF v_result IS NULL THEN
                v_unknown := true;
            ELSIF v_result = (v_op = 'or') THEN
                RETURN v_result;
            END IF;
        END LOOP;
        RETURN CASE WHEN v_unknown THEN NULL ELSE v_op = 'and' END;
    ELSIF v_op = 'not' THEN
        IF jsonb_typeof(p_plan->'args') IS DISTINCT FROM 'array' OR jsonb_array_length(p_plan->'args') <> 1 THEN
            RETURN NULL;
        END IF;
        RETURN NOT mandate.condition_holds(p_plan->'args'->0, p_action, p_payload);
    ELSIF v_op IN ('==', '!=', '<', '<=', '>', '>=') THEN
        RETURN mandate.condition_compare(v_op, mandate.condition_value(p_plan->'path', p_action, p_payload), p_plan->'value');
    ELSIF v_op = 'in' THEN
        v_value := mandate.condition_value(p_plan->'path', p_action, p_payload);
        IF v_value IS NULL OR jsonb_typeof(p_plan->'values') IS DISTINCT FROM 'array' OR jsonb_array_length(p_plan->'values') = 0 THEN
            RETURN NULL;
        END IF;
        v_result := false;
        FOR v_arg IN SELECT a FROM jsonb_array_elements(p_plan->'values') AS a LOOP
            IF jsonb_typeof(v_arg) IS DISTINCT FROM jsonb_typeof(v_value) THEN
                RETURN NULL;
            END IF;
            -- Values of a kind that does not compare (null, objects, arrays)
            -- make the answer unknown, never a plain no.
            CASE mandate.condition_compare('==', v_value, v_arg)
                WHEN true THEN v_result := true;
                WHEN false THEN NULL;
                ELSE RETURN NULL;
            END CASE;
        END LOOP;
        RETURN v_result;
    ELSIF v_op = 'has' THEN
        IF jsonb_typeof(p_plan->'path') IS DISTINCT FROM 'array' THEN
            RETURN NULL;
        END IF;
        v_length := jsonb_array_length(p_plan->'path');
        IF v_length < 2 OR p_plan->'path'->>0 IS DISTINCT FROM 'payload'
            OR jsonb_typeof(p_plan->'path'->(v_length - 1)) IS DISTINCT FROM 'string' THEN
            RETURN NULL;
        END IF;
        -- The parent of a top level field is the payload itself.
        v_parent := CASE WHEN v_length = 2 THEN p_payload
            ELSE mandate.condition_value((p_plan->'path') - (v_length - 1), p_action, p_payload) END;
        IF jsonb_typeof(v_parent) IS DISTINCT FROM 'object' THEN
            RETURN NULL;
        END IF;
        RETURN v_parent ? (p_plan->'path'->>(v_length - 1));
    END IF;
    RETURN NULL;
END
$$;

GRANT EXECUTE ON FUNCTION mandate.condition_value(jsonb, text, jsonb), mandate.condition_compare(text, jsonb, jsonb),
    mandate.condition_holds(jsonb, text, jsonb) TO mandate_app;

-- A mandate that is live now, for this agent (which must not be revoked),
-- that covers the action at the risk the basis claims, whose condition is
-- the one the basis cites and holds for the payload executed, and whose
-- budget and count are what the basis says, with room for one more. The
-- execution trigger checks the basis against the tables, so an execution
-- cannot cite a mandate that does not hold or a budget that is not there.
-- The basis carries its budgets as a list; a count is the only kind yet,
-- and the list must hold exactly that one.
CREATE FUNCTION mandate.basis_holds(p_tenant uuid, p_agent uuid, p_action text, p_payload jsonb, p_basis jsonb) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM mandate.mandates m
        JOIN mandate.agents a ON a.id = p_agent AND a.tenant_id = p_tenant AND a.revoked_at IS NULL
        WHERE m.id = (p_basis->>'mandateId')::uuid
            -- The id as text must be the canonical form, since the spend is
            -- counted by comparing it as text.
            AND p_basis->>'mandateId' = m.id::text
            AND m.tenant_id = p_tenant
            AND (m.agent_id IS NULL OR m.agent_id = p_agent)
            AND m.revoked_at IS NULL
            AND now() >= m.valid_from AND now() < m.valid_until
            AND m.max_risk = p_basis->>'maxRisk'
            AND m.policy_ref = p_basis->>'policyRef'
            AND jsonb_typeof(p_basis->'budgets') = 'array'
            AND jsonb_array_length(p_basis->'budgets') = 1
            AND p_basis->'budgets'->0->>'kind' = 'count'
            AND jsonb_typeof(p_basis->'budgets'->0->'limit') = 'number'
            AND jsonb_typeof(p_basis->'budgets'->0->'spent') = 'number'
            AND (p_basis->'budgets'->0->>'limit')::numeric = m.budget_count
            AND (p_basis->'budgets'->0->>'spent')::numeric = mandate.spent_count(m.id)
            AND mandate.spent_count(m.id) < m.budget_count
            AND mandate.covers(m.actions, p_action)
            AND m.condition IS NOT DISTINCT FROM p_basis->>'condition'
            AND (m.condition_plan IS NULL OR mandate.condition_holds(m.condition_plan, p_action, p_payload) IS TRUE)
            -- A defined action must allow execution and not be removed.
            AND NOT EXISTS (
                SELECT 1 FROM mandate.actions x
                WHERE x.tenant_id = p_tenant AND x.key = p_action AND (NOT x.may_execute OR x.removed_at IS NOT NULL))
    )
$$;
GRANT EXECUTE ON FUNCTION mandate.basis_holds(uuid, uuid, text, jsonb, jsonb) TO mandate_app;

CREATE FUNCTION mandate.proposals_on_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'proposal % is already decided', OLD.id USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- The deadline cuts both ways. From expires_at on, the only change is
    -- the system expiring it, with nothing else changed; before it, nothing
    -- expires it. now() is the transaction's start, so a decision whose
    -- transaction began before the deadline is in time.
    IF now() >= OLD.expires_at THEN
        IF NEW.status <> 'expired' OR mandate.current_actor_kind() IS DISTINCT FROM 'system'
            OR (to_jsonb(NEW) - 'status' - 'decided_at' - 'decided_by')
                <> (to_jsonb(OLD) - 'status' - 'decided_at' - 'decided_by') THEN
            RAISE EXCEPTION 'proposal % has expired: nobody decided it in time', OLD.id USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.decided_by := NULL;
        NEW.decided_at := now();
        RETURN NEW;
    END IF;
    IF NEW.status = 'expired' THEN
        RAISE EXCEPTION 'proposal % has not reached its deadline', OLD.id USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- The agent that proposed it may withdraw a waiting proposal, with a
    -- reason in the note, and change nothing else.
    IF NEW.status = 'withdrawn' THEN
        IF mandate.current_actor_kind() IS DISTINCT FROM 'agent' OR mandate.current_actor() IS DISTINCT FROM OLD.agent_id
            OR (to_jsonb(NEW) - 'status' - 'decided_at' - 'decided_by' - 'decision_note')
                <> (to_jsonb(OLD) - 'status' - 'decided_at' - 'decided_by' - 'decision_note') THEN
            RAISE EXCEPTION 'only the agent that proposed it withdraws a waiting proposal, and nothing else changes with it'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.decided_by := NULL;
        NEW.decided_at := now();
        RETURN NEW;
    END IF;
    IF NEW.status = 'executed' THEN
        -- The mandate row is locked, so two executions under it count the
        -- budget one after the other whatever the caller did.
        PERFORM 1 FROM mandate.mandates m
            WHERE m.id = CASE WHEN NEW.execution_basis->>'mandateId' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                THEN (NEW.execution_basis->>'mandateId')::uuid END
                AND m.tenant_id = OLD.tenant_id
            FOR UPDATE;
        -- The budget is counted after the mandate's lock, which holds only
        -- when each statement sees what committed before it.
        IF current_setting('transaction_isolation') <> 'read committed' THEN
            RAISE EXCEPTION 'a proposal is executed under read committed isolation only, so the budget is counted after the lock'
                USING ERRCODE = 'feature_not_supported';
        END IF;
        IF mandate.current_actor_kind() IS DISTINCT FROM 'system'
            OR mandate.executing_proposal() IS DISTINCT FROM OLD.id
            OR OLD.risk = 'high'
            OR cardinality(OLD.flags) > 0
            OR NEW.execution_basis IS NULL
            OR coalesce(NEW.execution_basis->>'maxRisk', '') NOT IN ('low', 'medium')
            OR (OLD.risk = 'medium' AND NEW.execution_basis->>'maxRisk' <> 'medium')
            OR NOT mandate.basis_holds(OLD.tenant_id, OLD.agent_id, OLD.action, OLD.payload, NEW.execution_basis)
            -- The action as it is now: a webhook action's risk raised above
            -- the proposal's leaves it to a person.
            OR EXISTS (
                SELECT 1 FROM mandate.actions x
                WHERE x.tenant_id = OLD.tenant_id AND x.key = OLD.action
                    AND x.connector_kind = 'webhook' AND mandate.risk_rank(x.risk) > mandate.risk_rank(OLD.risk))
            OR (to_jsonb(NEW) - 'status' - 'decided_at' - 'decided_by' - 'execution_basis')
                <> (to_jsonb(OLD) - 'status' - 'decided_at' - 'decided_by' - 'execution_basis') THEN
            RAISE EXCEPTION 'a proposal is executed only by the system inside its own execution, under a live mandate that covers the agent, the action and the risk, with room in its budget, whose condition holds for the payload, and never with flags'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.decided_by := NULL;
        NEW.decided_at := now();
        RETURN NEW;
    END IF;
    IF mandate.current_role() IS NULL THEN
        RAISE EXCEPTION 'only a person edits or decides a proposal' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- A person's update is a decision. A proposal left pending is left
    -- untouched, so nothing a person wrote can ride along when a mandate
    -- later allows it.
    IF NEW.status NOT IN ('approved', 'rejected') THEN
        RAISE EXCEPTION 'a person approves or rejects' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- A person's edit may raise the risk (the action assessed the edited
    -- payload), never lower it, and only on an approval.
    IF NEW.risk <> OLD.risk AND NOT (NEW.status = 'approved' AND mandate.risk_rank(NEW.risk) > mandate.risk_rank(OLD.risk)) THEN
        RAISE EXCEPTION 'a proposal''s risk can only be raised by the person approving an edit' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.agent_id <> OLD.agent_id OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.action <> OLD.action OR NEW.expires_at <> OLD.expires_at OR NEW.deliver_within <> OLD.deliver_within
        OR NEW.flags IS DISTINCT FROM OLD.flags OR NEW.idempotency_key <> OLD.idempotency_key
        OR NEW.request_hash <> OLD.request_hash OR NEW.proposed_payload IS DISTINCT FROM OLD.proposed_payload
        OR NEW.created_at <> OLD.created_at OR NEW.execution_basis IS DISTINCT FROM OLD.execution_basis THEN
        RAISE EXCEPTION 'immutable proposal fields changed' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.payload IS DISTINCT FROM OLD.payload OR NEW.title IS DISTINCT FROM OLD.title THEN
        -- An edit belongs to an approval. Otherwise a person could change a
        -- pending proposal and leave it for a mandate to execute, with no
        -- decision of theirs on record.
        IF NEW.status <> 'approved' THEN
            RAISE EXCEPTION 'a proposal is edited only as part of approving it' USING ERRCODE = 'insufficient_privilege';
        END IF;
    END IF;
    -- Whether it was edited, who decided and when are the database's to say.
    NEW.edited := NEW.payload IS DISTINCT FROM OLD.payload OR NEW.title IS DISTINCT FROM OLD.title;
    NEW.decided_by := mandate.current_actor();
    NEW.decided_at := now();
    RETURN NEW;
END
$$;

CREATE TRIGGER proposals_insert BEFORE INSERT ON mandate.proposals
    FOR EACH ROW EXECUTE FUNCTION mandate.proposals_on_insert();
CREATE TRIGGER proposals_update BEFORE UPDATE ON mandate.proposals
    FOR EACH ROW EXECUTE FUNCTION mandate.proposals_on_update();
CREATE TRIGGER proposals_no_delete BEFORE DELETE ON mandate.proposals
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

-- Every new proposal states why. Its own trigger, beside proposals_insert.
CREATE FUNCTION mandate.proposals_require_rationale() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.rationale IS NULL OR NEW.rationale !~ '\S' THEN
        RAISE EXCEPTION 'a proposal states its rationale' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER proposals_require_rationale BEFORE INSERT ON mandate.proposals
    FOR EACH ROW EXECUTE FUNCTION mandate.proposals_require_rationale();

-- The account is the agent's, as it was given: no one edits it, a person
-- deciding included. A person's view goes in the decision note.
CREATE FUNCTION mandate.proposals_keep_account() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.rationale IS DISTINCT FROM OLD.rationale OR NEW.evidence IS DISTINCT FROM OLD.evidence
        OR NEW.provenance IS DISTINCT FROM OLD.provenance THEN
        RAISE EXCEPTION 'the agent''s account of a proposal is never changed' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER proposals_keep_account BEFORE UPDATE ON mandate.proposals
    FOR EACH ROW EXECUTE FUNCTION mandate.proposals_keep_account();


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


-- A payload whose object has two keys that differ only in case, such as
-- amountCents and amountcents, is refused. The database reads a key
-- exactly, and a condition is checked on that key; many JSON decoders
-- (Go's encoding/json among them) match keys without regard to case and
-- take the last, so a receiver could act on the other value. Two Unicode
-- letters that such decoders fold to ASCII, the long s and the Kelvin
-- sign, are refused in keys for the same reason.
CREATE FUNCTION mandate.keys_unambiguous(p_value jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT NOT EXISTS (
        SELECT 1 FROM jsonb_path_query(p_value, 'strict $.**') AS o
        WHERE jsonb_typeof(o) = 'object' AND (
            EXISTS (SELECT 1 FROM jsonb_object_keys(o) AS k WHERE k ~ '[ſK]')
            OR (SELECT count(*) FROM jsonb_object_keys(o)) <> (SELECT count(DISTINCT lower(k)) FROM jsonb_object_keys(o) AS k)))
$$;
GRANT EXECUTE ON FUNCTION mandate.keys_unambiguous(jsonb) TO mandate_app;

-- A trigger rather than a check: a check would hold every later update of
-- a row to it, and a proposal stored before this migration with such a
-- payload could then never be decided. The payload is checked when it is
-- written: on insert, and when an edit changes it.
CREATE FUNCTION mandate.proposals_payload_keys() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (TG_OP = 'INSERT' OR NEW.payload IS DISTINCT FROM OLD.payload) AND NOT mandate.keys_unambiguous(NEW.payload) THEN
        RAISE EXCEPTION 'a payload key could be read as another: two keys differ only in case, or one has a letter decoders fold into ASCII'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER proposals_payload_keys BEFORE INSERT OR UPDATE OF payload ON mandate.proposals
    FOR EACH ROW EXECUTE FUNCTION mandate.proposals_payload_keys();


------------------------------------------------------------------------------
-- Deliveries
------------------------------------------------------------------------------
-- A decided proposal whose action is a webhook is delivered to its
-- receiver by a job. The decision stands whatever happens here. Delivery
-- contract v2 (docs/design-0.1.md): only a receiver's explicit answer is
-- final; an attempt counts as possibly sent from the moment it opens until
-- it is proven unsent; a delivery that may or may not have happened is
-- settled by a person, and only once the work can no longer happen.
CREATE TABLE mandate.deliveries (
    id               uuid PRIMARY KEY,
    tenant_id        uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    proposal_id      uuid NOT NULL,
    action_key       text NOT NULL,
    url              text NOT NULL,
    audience         text NOT NULL,
    status           text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'done', 'refused', 'not_sent', 'unknown', 'settled_done', 'settled_not_done')),
    -- The time after which it must not be carried out: the decision's time
    -- plus the proposal's delivery window. Set by the insert trigger.
    not_after        timestamptz NOT NULL,
    -- Attempts opened, and those proven never to have reached the receiver.
    -- An attempt is possibly sent from the moment it opens.
    attempts_started int NOT NULL DEFAULT 0 CHECK (attempts_started >= 0),
    attempts_unsent  int NOT NULL DEFAULT 0 CHECK (attempts_unsent >= 0 AND attempts_unsent <= attempts_started),
    attempt_open_at  timestamptz,
    last_status      int,
    last_error       text CHECK (last_error IS NULL OR length(last_error) <= 1000),
    -- The receiver's result, or its refusal, when it gave one.
    result           jsonb CHECK (result IS NULL OR (jsonb_typeof(result) = 'object' AND octet_length(result::text) <= 16384)),
    created_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz,
    settled_by       uuid,
    UNIQUE (tenant_id, proposal_id),
    FOREIGN KEY (tenant_id, proposal_id) REFERENCES mandate.proposals (tenant_id, id) ON DELETE CASCADE
);

-- For the sweep: deliveries that may be due.
CREATE INDEX deliveries_due_idx ON mandate.deliveries (not_after) WHERE status = 'pending';

ALTER TABLE mandate.deliveries ENABLE ROW LEVEL SECURITY;
CREATE POLICY deliveries_scope ON mandate.deliveries
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT SELECT, INSERT, UPDATE ON mandate.deliveries TO mandate_app;

-- The request timeout and the allowance for clocks, as the contract states
-- them: no attempt opens later than not_after minus the timeout, an attempt
-- open longer than the timeout and a margin was left by a crash, and a
-- person settles only once the work can no longer happen.
CREATE FUNCTION mandate.delivery_timeout() RETURNS interval LANGUAGE sql IMMUTABLE AS $$ SELECT interval '15 seconds' $$;
CREATE FUNCTION mandate.delivery_skew() RETURNS interval LANGUAGE sql IMMUTABLE AS $$ SELECT interval '5 minutes' $$;
CREATE FUNCTION mandate.delivery_stale() RETURNS interval LANGUAGE sql IMMUTABLE AS $$ SELECT interval '45 seconds' $$;
GRANT EXECUTE ON FUNCTION mandate.delivery_timeout(), mandate.delivery_skew(), mandate.delivery_stale() TO mandate_app;

CREATE FUNCTION mandate.deliveries_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_possibly_sent boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- Recorded in the deciding transaction: a person's approval or the
        -- system's execution.
        IF mandate.current_role() IS NULL AND mandate.current_actor_kind() IS DISTINCT FROM 'system' THEN
            RAISE EXCEPTION 'a delivery is recorded by the deciding person or the system' USING ERRCODE = 'insufficient_privilege';
        END IF;
        -- Only a decided proposal is delivered, only to its action's own
        -- connector and audience, and only in the transaction that decides
        -- it: an old decision is never sent to a connector defined after it.
        SELECT p.decided_at + p.deliver_within INTO NEW.not_after
            FROM mandate.proposals p
            JOIN mandate.actions a ON a.tenant_id = p.tenant_id AND a.key = p.action
            WHERE p.id = NEW.proposal_id AND p.tenant_id = NEW.tenant_id AND p.status IN ('approved', 'executed')
                AND p.decided_at = now()
                AND a.connector_kind = 'webhook' AND a.removed_at IS NULL
                AND a.connector_url = NEW.url AND a.connector_audience = NEW.audience AND p.action = NEW.action_key;
        IF NEW.not_after IS NULL THEN
            RAISE EXCEPTION 'a delivery is recorded in the transaction that decides its proposal, and goes to its action''s connector' USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.status := 'pending';
        NEW.attempts_started := 0;
        NEW.attempts_unsent := 0;
        NEW.attempt_open_at := NULL;
        NEW.last_status := NULL;
        NEW.last_error := NULL;
        NEW.result := NULL;
        NEW.created_at := now();
        NEW.finished_at := NULL;
        NEW.settled_by := NULL;
        RETURN NEW;
    END IF;

    IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id OR NEW.proposal_id <> OLD.proposal_id
        OR NEW.action_key <> OLD.action_key OR NEW.not_after <> OLD.not_after OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a delivery keeps its proposal and its deadline' USING ERRCODE = 'insufficient_privilege';
    END IF;
    v_possibly_sent := OLD.attempts_started > OLD.attempts_unsent OR OLD.attempt_open_at IS NOT NULL;

    -- A person settles a delivery whose outcome is unknown, once the work
    -- can no longer happen, and changes nothing else.
    IF OLD.status = 'unknown' AND NEW.status IN ('settled_done', 'settled_not_done') THEN
        IF mandate.current_role() IS NULL
            OR now() < OLD.not_after + mandate.delivery_timeout() + mandate.delivery_skew()
            OR (to_jsonb(NEW) - 'status' - 'settled_by' - 'finished_at') <> (to_jsonb(OLD) - 'status' - 'settled_by' - 'finished_at') THEN
            RAISE EXCEPTION 'a person settles an unknown delivery only once the work can no longer happen' USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.settled_by := mandate.current_actor();
        NEW.finished_at := now();
        RETURN NEW;
    END IF;

    -- A person sends a delivery that was never sent again, before it is too
    -- late, to the action's connector and audience as they are now: same
    -- delivery, same counters.
    IF OLD.status = 'not_sent' AND NEW.status = 'pending' THEN
        IF mandate.current_role() IS NULL
            OR now() > OLD.not_after - mandate.delivery_timeout()
            OR (to_jsonb(NEW) - 'status' - 'url' - 'audience' - 'finished_at') <> (to_jsonb(OLD) - 'status' - 'url' - 'audience' - 'finished_at')
            OR NOT EXISTS (
                SELECT 1 FROM mandate.actions a
                WHERE a.tenant_id = NEW.tenant_id AND a.key = NEW.action_key AND a.removed_at IS NULL
                    AND a.connector_kind = 'webhook' AND a.connector_url = NEW.url AND a.connector_audience = NEW.audience) THEN
            RAISE EXCEPTION 'a person sends again only a delivery never sent, before its deadline, to its action''s connector' USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.finished_at := NULL;
        RETURN NEW;
    END IF;

    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'a delivery that has left pending changes only by a person settling or sending it again' USING ERRCODE = 'insufficient_privilege';
    END IF;

    -- A move to the action's new address and audience: by the owner moving
    -- the action, or the system finding it behind. Never once an attempt
    -- may have reached the old receiver.
    IF NEW.url <> OLD.url OR NEW.audience <> OLD.audience THEN
        IF (mandate.current_role() IS DISTINCT FROM 'owner' AND mandate.current_actor_kind() IS DISTINCT FROM 'system')
            OR v_possibly_sent
            OR (to_jsonb(NEW) - 'url' - 'audience') <> (to_jsonb(OLD) - 'url' - 'audience')
            OR NOT EXISTS (
                SELECT 1 FROM mandate.actions a
                WHERE a.tenant_id = NEW.tenant_id AND a.key = NEW.action_key AND a.removed_at IS NULL
                    AND a.connector_kind = 'webhook' AND a.connector_url = NEW.url AND a.connector_audience = NEW.audience) THEN
            RAISE EXCEPTION 'a waiting delivery moves only to its action''s connector, and never once it may have been sent' USING ERRCODE = 'insufficient_privilege';
        END IF;
        RETURN NEW;
    END IF;

    -- Everything else is the system's: opening and closing attempts, and the
    -- outcome.
    IF mandate.current_actor_kind() IS DISTINCT FROM 'system' THEN
        RAISE EXCEPTION 'only the system records delivery attempts' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.attempts_started < OLD.attempts_started OR NEW.attempts_unsent < OLD.attempts_unsent
        OR NEW.attempts_started > OLD.attempts_started + 1 OR NEW.attempts_unsent > OLD.attempts_unsent + 1 THEN
        RAISE EXCEPTION 'attempts are counted one at a time, and never back' USING ERRCODE = 'insufficient_privilege';
    END IF;

    -- Opening an attempt: one more, open now, never too late, and never
    -- over an attempt still open unless that one was left by a crash.
    IF NEW.attempts_started = OLD.attempts_started + 1 THEN
        IF NEW.status <> 'pending' OR NEW.attempts_unsent <> OLD.attempts_unsent
            OR now() > OLD.not_after - mandate.delivery_timeout()
            OR (OLD.attempt_open_at IS NOT NULL AND OLD.attempt_open_at > now() - mandate.delivery_stale()) THEN
            RAISE EXCEPTION 'an attempt opens only while the delivery waits, before its deadline, and with none open' USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.attempt_open_at := now();
        RETURN NEW;
    END IF;

    -- Proving an attempt unsent closes it.
    IF NEW.attempts_unsent = OLD.attempts_unsent + 1 AND (OLD.attempt_open_at IS NULL OR NEW.attempt_open_at IS NOT NULL) THEN
        RAISE EXCEPTION 'only an open attempt is proven unsent, and that closes it' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- An explicit answer comes with the attempt it answers.
    IF NEW.status IN ('done', 'refused') OR (NEW.status = 'unknown' AND NEW.result IS NOT NULL) THEN
        IF OLD.attempt_open_at IS NULL OR NEW.attempt_open_at IS NOT NULL THEN
            RAISE EXCEPTION 'a receiver''s answer is recorded with the attempt it answers' USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.finished_at := now();
        RETURN NEW;
    END IF;
    -- Finishing without an answer: no attempt open, and not_sent only if no
    -- attempt was possibly sent.
    IF NEW.status IN ('unknown', 'not_sent') THEN
        IF OLD.attempt_open_at IS NOT NULL OR NEW.attempt_open_at IS NOT NULL
            OR (NEW.status = 'not_sent' AND NEW.attempts_started > NEW.attempts_unsent)
            OR (NEW.status = 'unknown' AND NEW.attempts_started = NEW.attempts_unsent) THEN
            RAISE EXCEPTION 'a delivery ends not sent only if no attempt was possibly sent, and with no attempt open' USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.finished_at := now();
        RETURN NEW;
    END IF;
    IF NEW.status <> 'pending' THEN
        RAISE EXCEPTION 'a delivery ends done, refused, unknown or not sent' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.result IS DISTINCT FROM OLD.result OR NEW.finished_at IS DISTINCT FROM OLD.finished_at OR NEW.settled_by IS DISTINCT FROM OLD.settled_by THEN
        RAISE EXCEPTION 'a waiting delivery has no result' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER deliveries_guard BEFORE INSERT OR UPDATE ON mandate.deliveries
    FOR EACH ROW EXECUTE FUNCTION mandate.deliveries_guard();
CREATE TRIGGER deliveries_no_delete BEFORE DELETE ON mandate.deliveries
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

------------------------------------------------------------------------------
-- Sessions
------------------------------------------------------------------------------
-- A person's session is global, not tenant scoped: they sign in once and
-- then pick a tenant where they are a person. Only the hash of the token
-- is stored.
CREATE TABLE mandate.sessions (
    token_hash    bytea PRIMARY KEY CHECK (length(token_hash) = 32),
    email         text NOT NULL CHECK (email = lower(email) AND email LIKE '%_@_%' AND length(email) <= 200),
    name          text NOT NULL DEFAULT '' CHECK (length(name) <= 200),
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    last_seen_at  timestamptz
);

CREATE INDEX sessions_email_idx ON mandate.sessions (email);

GRANT SELECT, INSERT, DELETE ON mandate.sessions TO mandate_app;
GRANT UPDATE (last_seen_at) ON mandate.sessions TO mandate_app;

-- The tenants where an email is a person, for the tenant picker after
-- sign-in. Runs as the owner because no tenant is in scope yet; it reads
-- only rows for the exact email given.
CREATE FUNCTION mandate.tenants_for_email(p_email text)
RETURNS TABLE (tenant_id uuid, tenant_name text, person_id uuid, role text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = mandate, pg_temp AS $$
    SELECT t.id, t.name, p.id, p.role
    FROM mandate.persons p
    JOIN mandate.tenants t ON t.id = p.tenant_id
    WHERE p.email = lower(p_email) AND p.removed_at IS NULL
    ORDER BY t.name
$$;
REVOKE ALL ON FUNCTION mandate.tenants_for_email(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mandate.tenants_for_email(text) TO mandate_app;

------------------------------------------------------------------------------
-- The event chain
------------------------------------------------------------------------------
-- Everything that happens, per tenant, append-only and hash chained: each
-- hash covers the previous hash and a canonical encoding of the event, so an
-- edit, a removal or a reordering breaks verification.
CREATE TABLE mandate.events (
    tenant_id   uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    seq         bigint NOT NULL CHECK (seq > 0),
    type        text NOT NULL CHECK (type ~ '^[a-z]+(\.[a-z_]+)+$'),
    actor_kind  text NOT NULL CHECK (actor_kind IN ('person', 'agent', 'system')),
    actor_id    uuid,
    subject_id  uuid,
    run_id      text,
    payload     jsonb NOT NULL,
    prev_hash   bytea NOT NULL CHECK (length(prev_hash) = 32),
    hash        bytea NOT NULL CHECK (length(hash) = 32),
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, seq)
);

CREATE INDEX events_subject_idx ON mandate.events (tenant_id, subject_id, seq);

ALTER TABLE mandate.events ENABLE ROW LEVEL SECURITY;
CREATE POLICY events_scope ON mandate.events
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

-- For "many approvals in a short time".
CREATE INDEX events_approvals_idx ON mandate.events (tenant_id, actor_id, created_at) WHERE type = 'proposal.approved';

-- The actor and the run of an event are the session's, never the caller's,
-- so nothing an agent does can be written as a person's. An event must
-- continue the chain: the next number and the last hash. The lock is the
-- one the application takes, so appends per tenant are one at a time
-- whoever makes them.
CREATE FUNCTION mandate.events_on_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_seq  bigint;
    v_hash bytea;
    v_at   timestamptz;
BEGIN
    IF mandate.current_actor_kind() IS NULL THEN
        RAISE EXCEPTION 'an event needs an actor' USING ERRCODE = 'insufficient_privilege';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id::text, 7));
    SELECT e.seq, e.hash, e.created_at INTO v_seq, v_hash, v_at FROM mandate.events e
        WHERE e.tenant_id = NEW.tenant_id ORDER BY e.seq DESC LIMIT 1;
    IF NEW.seq IS DISTINCT FROM coalesce(v_seq, 0) + 1
        OR NEW.prev_hash IS DISTINCT FROM coalesce(v_hash, decode(repeat('00', 32), 'hex')) THEN
        RAISE EXCEPTION 'an event must continue the chain' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- The time is part of the hash, so the application sets it, from this
    -- database's clock. It never goes back, and is never ahead of the clock
    -- unless it holds at the previous event's time while the clock, stepped
    -- back, catches up.
    IF NEW.created_at < v_at OR (NEW.created_at > clock_timestamp() AND NEW.created_at IS DISTINCT FROM v_at) THEN
        RAISE EXCEPTION 'an event''s time follows the one before it and is not in the future' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.actor_kind IS DISTINCT FROM mandate.current_actor_kind()
        OR NEW.actor_id IS DISTINCT FROM mandate.current_actor()
        OR NEW.run_id IS DISTINCT FROM mandate.current_run() THEN
        RAISE EXCEPTION 'an event carries the actor and the run of its session' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER events_insert BEFORE INSERT ON mandate.events
    FOR EACH ROW EXECUTE FUNCTION mandate.events_on_insert();
CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON mandate.events
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

GRANT SELECT, INSERT ON mandate.events TO mandate_app;

------------------------------------------------------------------------------
-- Owner-only maintenance
------------------------------------------------------------------------------
-- Deletes a tenant and everything that cascades from it, including
-- append-only rows. Not granted to the app.
CREATE FUNCTION mandate.purge_tenant(p_tenant uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = mandate, pg_temp AS $$
BEGIN
    PERFORM set_config('mandate.allow_purge', 'on', true);
    DELETE FROM mandate.tenants WHERE id = p_tenant;
    PERFORM set_config('mandate.allow_purge', 'off', true);
END
$$;
REVOKE ALL ON FUNCTION mandate.purge_tenant(uuid) FROM PUBLIC;

-- Lists tenant ids only, for system jobs that run per tenant. The job then
-- scopes itself to each tenant; no data crosses tenants.
CREATE FUNCTION mandate.all_tenant_ids() RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = mandate, pg_temp AS $$
    SELECT id FROM mandate.tenants ORDER BY id
$$;
REVOKE ALL ON FUNCTION mandate.all_tenant_ids() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mandate.all_tenant_ids() TO mandate_app;

-- The tenants with work the sweep must do now: a waiting proposal past its
-- deadline, a waiting delivery past the last moment an attempt may open
-- with none open, or an attempt left open by a crash. Ids only; the sweep
-- then scopes itself to each tenant.
CREATE FUNCTION mandate.tenants_with_due_work() RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = mandate, pg_temp AS $$
    SELECT p.tenant_id FROM mandate.proposals p
    WHERE p.status = 'pending' AND p.expires_at <= now()
    UNION
    SELECT d.tenant_id FROM mandate.deliveries d
    WHERE d.status = 'pending' AND (
        (d.attempt_open_at IS NULL AND d.not_after - mandate.delivery_timeout() <= now())
        OR d.attempt_open_at <= now() - mandate.delivery_stale())
$$;
REVOKE ALL ON FUNCTION mandate.tenants_with_due_work() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mandate.tenants_with_due_work() TO mandate_app;

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
