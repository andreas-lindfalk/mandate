-- Back to 0001: mandates without conditions. A mandate that still holds
-- with a condition would hold without it afterwards and allow more than
-- was granted, so the rollback is refused until those are revoked. The
-- table is locked first, so none is granted between the check and the drop.

BEGIN;

LOCK TABLE mandate.mandates IN SHARE ROW EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM mandate.mandates WHERE condition IS NOT NULL AND revoked_at IS NULL AND valid_until > now()) THEN
        RAISE EXCEPTION 'mandates with a condition still hold: revoke them before rolling back 0002, or they would allow more than was granted'
            USING ERRCODE = 'object_in_use';
    END IF;
END
$$;

DROP FUNCTION mandate.basis_holds(uuid, uuid, text, bigint, jsonb, jsonb);

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

DROP FUNCTION mandate.condition_holds(jsonb, text, jsonb);
DROP FUNCTION mandate.condition_compare(text, jsonb, jsonb);
DROP FUNCTION mandate.condition_value(jsonb, text, jsonb);

ALTER TABLE mandate.mandates DROP COLUMN condition, DROP COLUMN condition_plan;
DROP FUNCTION mandate.valid_condition_plan(jsonb);
DROP FUNCTION mandate.valid_condition_node(jsonb, int);
DROP FUNCTION mandate.valid_condition_path(jsonb);

COMMIT;
