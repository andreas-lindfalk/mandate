-- Back to 0006: no withdrawal. Withdrawn proposals are recorded as
-- rejected first, so the narrower status check holds; the guard that keeps
-- decided proposals unchanged is set aside for that one update (the
-- migration runs as the table's owner).

ALTER TABLE mandate.proposals DISABLE TRIGGER proposals_update;
UPDATE mandate.proposals SET status = 'rejected' WHERE status = 'withdrawn';
ALTER TABLE mandate.proposals ENABLE TRIGGER proposals_update;

ALTER TABLE mandate.proposals DROP CONSTRAINT proposals_status_check;
ALTER TABLE mandate.proposals ADD CONSTRAINT proposals_status_check
    CHECK (status IN ('pending', 'approved', 'rejected', 'executed'));

-- As in 0002.
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
