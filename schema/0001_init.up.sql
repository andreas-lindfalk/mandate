-- mandate: agents propose, the database decides.
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
    IF TG_OP = 'DELETE' AND current_setting('mandate.allow_purge', true) = 'on' THEN
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
    IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL
        OR (to_jsonb(NEW) - 'revoked_at' - 'revoked_by') <> (to_jsonb(OLD) - 'revoked_at' - 'revoked_by') THEN
        RAISE EXCEPTION 'an agent can only be revoked, once' USING ERRCODE = 'insufficient_privilege';
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
    budget_units  bigint NOT NULL CHECK (budget_units > 0 AND budget_units <= 1000000000000),
    policy_ref    text NOT NULL CHECK (length(policy_ref) BETWEEN 1 AND 200),
    department    text CHECK (department IS NULL OR length(department) BETWEEN 1 AND 100),
    granted_by    uuid,
    valid_from    timestamptz NOT NULL DEFAULT now(),
    valid_until   timestamptz NOT NULL,
    revoked_at    timestamptz,
    revoked_by    uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- The difference of two instants is exact, whatever the session's time zone.
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
-- schema, a default risk, a cost, whether a mandate may ever let it run
-- without a person, and the connector that carries it out: a signed
-- webhook, or builtin code in an embedded host. A removed action keeps its
-- row, so old proposals still read.
CREATE TABLE mandate.actions (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    key               text NOT NULL CHECK (mandate.valid_action_key(key)),
    description       text NOT NULL CHECK (length(description) <= 1000),
    risk              text NOT NULL CHECK (risk IN ('low', 'medium', 'high')),
    cost_units        bigint NOT NULL CHECK (cost_units >= 0 AND cost_units <= 1000000000),
    may_execute       boolean NOT NULL DEFAULT false,
    schema            jsonb CHECK (schema IS NULL OR jsonb_typeof(schema) = 'object'),
    connector_kind    text NOT NULL CHECK (connector_kind IN ('webhook', 'builtin')),
    connector_url     text CHECK (connector_url IS NULL OR (length(connector_url) <= 2000 AND connector_url ~ '^https?://')),
    connector_secret  bytea CHECK (connector_secret IS NULL OR length(connector_secret) BETWEEN 16 AND 256),
    department        text CHECK (department IS NULL OR length(department) BETWEEN 1 AND 100),
    created_by        uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_by        uuid,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    removed_at        timestamptz,
    UNIQUE (tenant_id, key),
    -- A webhook has an address and a secret; a builtin action has neither,
    -- so a secret cannot survive a switch to builtin and back.
    CHECK ((connector_kind = 'webhook') = (connector_url IS NOT NULL)),
    CHECK ((connector_kind = 'webhook') = (connector_secret IS NOT NULL)),
    -- What is delivered to another system is held to a schema.
    CHECK (connector_kind <> 'webhook' OR schema IS NOT NULL)
);

ALTER TABLE mandate.actions ENABLE ROW LEVEL SECURITY;
CREATE POLICY actions_scope ON mandate.actions
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

-- The application reads every column but the connector's secret. Only
-- the system, sending a delivery, gets it, through connector_of below: a
-- transaction scoped as an agent or a person cannot read it.
GRANT INSERT, UPDATE ON mandate.actions TO mandate_app;
GRANT SELECT (id, tenant_id, key, description, risk, cost_units, may_execute, schema, connector_kind, connector_url,
    department, created_by, created_at, updated_by, updated_at, removed_at) ON mandate.actions TO mandate_app;

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
    -- A webhook that moves to a new address gets a new secret: the receiver
    -- at the old one holds the old secret and must not sign for the new.
    IF TG_OP = 'UPDATE' AND NEW.connector_kind = 'webhook' AND OLD.connector_kind = 'webhook'
        AND NEW.connector_url IS DISTINCT FROM OLD.connector_url
        AND NEW.connector_secret IS NOT DISTINCT FROM OLD.connector_secret THEN
        RAISE EXCEPTION 'a webhook moved to a new address needs a new secret' USING ERRCODE = 'insufficient_privilege';
    END IF;
    NEW.updated_by := mandate.current_actor();
    NEW.updated_at := now();
    RETURN NEW;
END
$$;

-- connector_of gives the system, and only the system, an action's
-- connector address and its secret together, as they are now, for the
-- tenant in scope.
CREATE FUNCTION mandate.connector_of(p_action text) RETURNS TABLE (url text, secret bytea)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = mandate, pg_temp AS $$
BEGIN
    IF mandate.current_actor_kind() IS DISTINCT FROM 'system' OR mandate.current_tenant() IS NULL THEN
        RAISE EXCEPTION 'only the system reads a connector''s secret' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN QUERY SELECT a.connector_url, a.connector_secret FROM mandate.actions a
        WHERE a.tenant_id = mandate.current_tenant() AND a.key = p_action
            AND a.connector_kind = 'webhook' AND a.removed_at IS NULL;
END
$$;
REVOKE ALL ON FUNCTION mandate.connector_of(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mandate.connector_of(text) TO mandate_app;

CREATE TRIGGER actions_guard BEFORE INSERT OR UPDATE ON mandate.actions
    FOR EACH ROW EXECUTE FUNCTION mandate.actions_guard();
CREATE TRIGGER actions_no_delete BEFORE DELETE ON mandate.actions
    FOR EACH ROW EXECUTE FUNCTION mandate.forbid_change();

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
    cost_units       bigint NOT NULL CHECK (cost_units >= 0),
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
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'executed')),
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

ALTER TABLE mandate.proposals ENABLE ROW LEVEL SECURITY;
CREATE POLICY proposals_scope ON mandate.proposals
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT SELECT, INSERT, UPDATE ON mandate.proposals TO mandate_app;

-- The proposer is always the agent in scope, never what the caller wrote.
CREATE FUNCTION mandate.proposals_on_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_removed_at timestamptz;
    v_cost       bigint;
    v_kind       text;
    v_risk       text;
BEGIN
    IF mandate.current_actor_kind() IS DISTINCT FROM 'agent' OR mandate.current_actor() IS NULL THEN
        RAISE EXCEPTION 'only an agent proposes' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM mandate.agents a
            WHERE a.id = mandate.current_actor() AND a.tenant_id = NEW.tenant_id AND a.revoked_at IS NULL) THEN
        RAISE EXCEPTION 'only a live agent of the tenant proposes' USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- Where the tenant has defined the action, the proposal is held to the
    -- definition: not removed, the definition's cost, and for a webhook
    -- action a risk no lower than the definition's. An action that exists
    -- only as code in an embedded host has no row to check against; there
    -- the host's code is the authority.
    SELECT a.removed_at, a.cost_units, a.connector_kind, a.risk INTO v_removed_at, v_cost, v_kind, v_risk
        FROM mandate.actions a WHERE a.tenant_id = NEW.tenant_id AND a.key = NEW.action;
    IF FOUND THEN
        IF v_removed_at IS NOT NULL THEN
            RAISE EXCEPTION 'the action % is removed', NEW.action USING ERRCODE = 'insufficient_privilege';
        END IF;
        IF NEW.cost_units <> v_cost THEN
            RAISE EXCEPTION 'a proposal carries the cost of its action' USING ERRCODE = 'insufficient_privilege';
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
    RETURN NEW;
END
$$;

-- What the executions under a mandate have cost in the current UTC
-- calendar month. The budget period is the database's clock.
CREATE FUNCTION mandate.spent_units(p_mandate uuid) RETURNS bigint
LANGUAGE sql STABLE AS $$
    SELECT coalesce(sum(p.cost_units), 0)::bigint FROM mandate.proposals p
    WHERE p.tenant_id = (SELECT m.tenant_id FROM mandate.mandates m WHERE m.id = p_mandate)
        AND p.status = 'executed' AND p.execution_basis->>'mandateId' = p_mandate::text
        AND p.decided_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
        AND p.decided_at < (date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC'
$$;
GRANT EXECUTE ON FUNCTION mandate.spent_units(uuid) TO mandate_app;

-- A mandate that is live now, for this agent (which must not be revoked),
-- that covers the action at the risk the basis claims, whose budget and
-- spend are what the basis says, and that still has room for the cost. The
-- execution trigger checks the basis against the tables, so an execution
-- cannot cite a mandate that does not hold or a budget that is not there.
CREATE FUNCTION mandate.basis_holds(p_tenant uuid, p_agent uuid, p_action text, p_cost bigint, p_basis jsonb) RETURNS boolean
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
            AND m.budget_units = (p_basis->>'budgetUnits')::bigint
            AND p_cost = (p_basis->>'costUnits')::bigint
            AND mandate.spent_units(m.id) = (p_basis->>'spentUnits')::bigint
            AND mandate.spent_units(m.id) + p_cost <= m.budget_units
            AND mandate.covers(m.actions, p_action)
            -- A defined action must allow execution and not be removed.
            AND NOT EXISTS (
                SELECT 1 FROM mandate.actions x
                WHERE x.tenant_id = p_tenant AND x.key = p_action AND (NOT x.may_execute OR x.removed_at IS NOT NULL))
    )
$$;
GRANT EXECUTE ON FUNCTION mandate.basis_holds(uuid, uuid, text, bigint, jsonb) TO mandate_app;

CREATE FUNCTION mandate.proposals_on_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'proposal % is already decided', OLD.id USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.status = 'executed' THEN
        -- The mandate row is locked, so two executions under it count the
        -- budget one after the other whatever the caller did.
        PERFORM 1 FROM mandate.mandates m
            WHERE m.id = CASE WHEN NEW.execution_basis->>'mandateId' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                THEN (NEW.execution_basis->>'mandateId')::uuid END
                AND m.tenant_id = OLD.tenant_id
            FOR UPDATE;
        IF mandate.current_actor_kind() IS DISTINCT FROM 'system'
            OR mandate.executing_proposal() IS DISTINCT FROM OLD.id
            OR OLD.risk = 'high'
            OR cardinality(OLD.flags) > 0
            OR NEW.execution_basis IS NULL
            OR coalesce(NEW.execution_basis->>'maxRisk', '') NOT IN ('low', 'medium')
            OR (OLD.risk = 'medium' AND NEW.execution_basis->>'maxRisk' <> 'medium')
            OR NOT mandate.basis_holds(OLD.tenant_id, OLD.agent_id, OLD.action, OLD.cost_units, NEW.execution_basis)
            -- The action as it is now: a cost changed since, or a webhook
            -- action's risk raised above the proposal's, leaves it to a person.
            OR EXISTS (
                SELECT 1 FROM mandate.actions x
                WHERE x.tenant_id = OLD.tenant_id AND x.key = OLD.action
                    AND (x.cost_units <> OLD.cost_units
                        OR (x.connector_kind = 'webhook' AND mandate.risk_rank(x.risk) > mandate.risk_rank(OLD.risk))))
            OR (to_jsonb(NEW) - 'status' - 'decided_at' - 'decided_by' - 'execution_basis')
                <> (to_jsonb(OLD) - 'status' - 'decided_at' - 'decided_by' - 'execution_basis') THEN
            RAISE EXCEPTION 'a proposal is executed only by the system inside its own execution, under a live mandate that covers the agent, the action, the risk and the cost within its budget, and never with flags'
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
        OR NEW.action <> OLD.action OR NEW.cost_units <> OLD.cost_units
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

------------------------------------------------------------------------------
-- Deliveries
------------------------------------------------------------------------------
-- A decided proposal whose action is a webhook is delivered to the
-- connector by a job. The decision stands whatever happens here; a failed
-- delivery shows in the chain and the UI, and a person can send it again.
-- The connector's answer is kept as the result, for the agent to read.
CREATE TABLE mandate.deliveries (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES mandate.tenants (id) ON DELETE CASCADE,
    proposal_id   uuid NOT NULL,
    action_key    text NOT NULL,
    url           text NOT NULL,
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts      int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_status   int,
    last_error    text CHECK (last_error IS NULL OR length(last_error) <= 1000),
    -- What the connector answered, when it answered with a JSON object.
    result        jsonb CHECK (result IS NULL OR (jsonb_typeof(result) = 'object' AND octet_length(result::text) <= 16384)),
    created_at    timestamptz NOT NULL DEFAULT now(),
    delivered_at  timestamptz,
    failed_at     timestamptz,
    UNIQUE (tenant_id, proposal_id),
    FOREIGN KEY (tenant_id, proposal_id) REFERENCES mandate.proposals (tenant_id, id) ON DELETE CASCADE
);

ALTER TABLE mandate.deliveries ENABLE ROW LEVEL SECURITY;
CREATE POLICY deliveries_scope ON mandate.deliveries
    USING (tenant_id = mandate.current_tenant())
    WITH CHECK (tenant_id = mandate.current_tenant());

GRANT SELECT, INSERT, UPDATE ON mandate.deliveries TO mandate_app;

CREATE FUNCTION mandate.deliveries_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- Recorded in the deciding transaction: a person's approval or the
        -- system's execution.
        IF mandate.current_role() IS NULL AND mandate.current_actor_kind() IS DISTINCT FROM 'system' THEN
            RAISE EXCEPTION 'a delivery is recorded by the deciding person or the system' USING ERRCODE = 'insufficient_privilege';
        END IF;
        -- Only a decided proposal is delivered, and only to its action's
        -- own connector.
        IF NOT EXISTS (
            SELECT 1 FROM mandate.proposals p
            JOIN mandate.actions a ON a.tenant_id = p.tenant_id AND a.key = p.action
            WHERE p.id = NEW.proposal_id AND p.tenant_id = NEW.tenant_id AND p.status IN ('approved', 'executed')
                -- decided in this very transaction: an old decision is
                -- never sent to a connector defined after it
                AND p.decided_at = now()
                AND a.connector_kind = 'webhook' AND a.connector_url = NEW.url AND p.action = NEW.action_key) THEN
            RAISE EXCEPTION 'a delivery is recorded in the transaction that decides its proposal, and goes to its action''s connector' USING ERRCODE = 'insufficient_privilege';
        END IF;
        NEW.status := 'pending';
        NEW.result := NULL;
        NEW.attempts := 0;
        NEW.delivered_at := NULL;
        NEW.failed_at := NULL;
        RETURN NEW;
    END IF;
    -- When an action's connector moves, its waiting deliveries move with it,
    -- to the new address and nothing else: the receiver at the old one no
    -- longer holds the secret they are signed with. The owner moves them in
    -- the same transaction; the system moves one it finds behind, about to
    -- send it.
    IF (mandate.current_role() = 'owner' OR mandate.current_actor_kind() = 'system')
        AND OLD.status = 'pending' AND NEW.status = 'pending'
        AND NEW.url <> OLD.url
        AND (to_jsonb(NEW) - 'url') = (to_jsonb(OLD) - 'url')
        AND EXISTS (
            SELECT 1 FROM mandate.actions a
            WHERE a.tenant_id = NEW.tenant_id AND a.key = NEW.action_key
                AND a.connector_kind = 'webhook' AND a.connector_url = NEW.url) THEN
        RETURN NEW;
    END IF;
    -- A person sends a failed delivery again: back to pending, to the
    -- action's connector as it is now, and nothing else changes.
    IF mandate.current_role() IS NOT NULL THEN
        IF OLD.status <> 'failed' OR NEW.status <> 'pending' OR NEW.failed_at IS NOT NULL
            OR (to_jsonb(NEW) - 'status' - 'failed_at' - 'url') <> (to_jsonb(OLD) - 'status' - 'failed_at' - 'url')
            OR NOT EXISTS (
                SELECT 1 FROM mandate.actions a
                WHERE a.tenant_id = NEW.tenant_id AND a.key = NEW.action_key
                    AND a.connector_kind = 'webhook' AND a.connector_url = NEW.url) THEN
            RAISE EXCEPTION 'a person can only send a failed delivery again, to its action''s connector' USING ERRCODE = 'insufficient_privilege';
        END IF;
        RETURN NEW;
    END IF;
    IF mandate.current_actor_kind() IS DISTINCT FROM 'system' THEN
        RAISE EXCEPTION 'only the system records delivery attempts' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.status <> 'pending' THEN
        RAISE EXCEPTION 'a delivery is final' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.proposal_id <> OLD.proposal_id OR NEW.action_key <> OLD.action_key OR NEW.url <> OLD.url OR NEW.tenant_id <> OLD.tenant_id THEN
        RAISE EXCEPTION 'a delivery keeps its proposal and its address' USING ERRCODE = 'insufficient_privilege';
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
