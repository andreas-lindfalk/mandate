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

ALTER TABLE mandate.mandates
    ADD COLUMN condition      text CHECK (condition IS NULL OR length(condition) BETWEEN 1 AND 2000),
    ADD COLUMN condition_plan jsonb CHECK (condition_plan IS NULL OR mandate.valid_condition_plan(condition_plan)),
    ADD CHECK ((condition IS NULL) = (condition_plan IS NULL));

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

-- basis_holds now sees the payload: the condition the basis cites must be
-- the mandate's own, and its plan must be true for the payload executed.
DROP FUNCTION mandate.basis_holds(uuid, uuid, text, bigint, jsonb);

CREATE FUNCTION mandate.basis_holds(p_tenant uuid, p_agent uuid, p_action text, p_cost bigint, p_payload jsonb, p_basis jsonb) RETURNS boolean
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
            AND m.condition IS NOT DISTINCT FROM p_basis->>'condition'
            AND (m.condition_plan IS NULL OR mandate.condition_holds(m.condition_plan, p_action, p_payload) IS TRUE)
            -- A defined action must allow execution and not be removed.
            AND NOT EXISTS (
                SELECT 1 FROM mandate.actions x
                WHERE x.tenant_id = p_tenant AND x.key = p_action AND (NOT x.may_execute OR x.removed_at IS NOT NULL))
    )
$$;
GRANT EXECUTE ON FUNCTION mandate.basis_holds(uuid, uuid, text, bigint, jsonb, jsonb) TO mandate_app;

-- As in 0001, with the payload passed to basis_holds.
CREATE OR REPLACE FUNCTION mandate.proposals_on_update() RETURNS trigger
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
            OR NOT mandate.basis_holds(OLD.tenant_id, OLD.agent_id, OLD.action, OLD.cost_units, OLD.payload, NEW.execution_basis)
            -- The action as it is now: a cost changed since, or a webhook
            -- action's risk raised above the proposal's, leaves it to a person.
            OR EXISTS (
                SELECT 1 FROM mandate.actions x
                WHERE x.tenant_id = OLD.tenant_id AND x.key = OLD.action
                    AND (x.cost_units <> OLD.cost_units
                        OR (x.connector_kind = 'webhook' AND mandate.risk_rank(x.risk) > mandate.risk_rank(OLD.risk))))
            OR (to_jsonb(NEW) - 'status' - 'decided_at' - 'decided_by' - 'execution_basis')
                <> (to_jsonb(OLD) - 'status' - 'decided_at' - 'decided_by' - 'execution_basis') THEN
            RAISE EXCEPTION 'a proposal is executed only by the system inside its own execution, under a live mandate that covers the agent, the action, the risk and the cost within its budget, whose condition holds for the payload, and never with flags'
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
