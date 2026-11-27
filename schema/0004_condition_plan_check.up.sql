-- 0001's check of a condition's plan let through plans it was meant to
-- refuse, in two ways. A test that fires random plans at it found both.
--
-- It could answer NULL. A node with a field missing, such as a comparison
-- with no path, made the check NULL rather than false, and a CHECK
-- constraint passes on NULL. Inside an and, or or not it was worse: the
-- node was skipped and the plan counted as valid.
--
-- It looked only at the fields a node's op uses. A comparison could carry
-- an "args" of any shape, which the service cannot read, so a plan the
-- database accepted could make the service fail on the mandate.
--
-- Neither let a mandate allow more than its plan says. A node with a field
-- missing evaluates to unknown, and unknown never allows; a comparison with
-- a stray field is evaluated as the comparison it is. But the check
-- promises that a stored plan has the shape the service compiles, and now
-- it does: every node carries exactly the fields of its op, and the check
-- answers true or false, never NULL.
--
-- Rows that exist are left as they are. The service only ever stored plans
-- its compiler wrote, and those have this shape. A row written past the
-- service may not, so the constraint is replaced as well (at the end): a
-- CHECK is held on every update of a row, a revocation included, and a
-- stricter check must never be what keeps a mandate from being revoked.
CREATE OR REPLACE FUNCTION mandate.valid_condition_path(p_path jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN jsonb_typeof(p_path) = 'array' THEN COALESCE(
        jsonb_array_length(p_path) BETWEEN 1 AND 11
        AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(p_path) AS s
            WHERE jsonb_typeof(s) IS DISTINCT FROM 'string' OR octet_length(s #>> '{}') NOT BETWEEN 1 AND 200)
        AND ((p_path->>0 = 'payload' AND jsonb_array_length(p_path) >= 2)
            OR (p_path->>0 = 'action' AND jsonb_array_length(p_path) = 1)), false)
        ELSE false END
$$;

CREATE OR REPLACE FUNCTION mandate.valid_condition_node(p_node jsonb, p_depth int) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    v_op    text;
    v_keys  text[];
    v_arg   jsonb;
    v_kind  text;
BEGIN
    IF p_depth > 64 OR jsonb_typeof(p_node) IS DISTINCT FROM 'object' OR jsonb_typeof(p_node->'op') IS DISTINCT FROM 'string' THEN
        RETURN false;
    END IF;
    v_op := p_node->>'op';
    -- The fields a node carries, to hold against the fields of its op.
    v_keys := ARRAY(SELECT k FROM jsonb_object_keys(p_node) AS k ORDER BY k COLLATE "C");
    IF v_op IN ('and', 'or', 'not') THEN
        -- The length is asked only of what is known to be an array: the
        -- order in which an OR is evaluated is not promised.
        IF v_keys <> ARRAY['args', 'op'] OR jsonb_typeof(p_node->'args') IS DISTINCT FROM 'array' THEN
            RETURN false;
        END IF;
        IF jsonb_array_length(p_node->'args') NOT BETWEEN 1 AND 100
            OR (v_op = 'not' AND jsonb_array_length(p_node->'args') <> 1) THEN
            RETURN false;
        END IF;
        FOR v_arg IN SELECT a FROM jsonb_array_elements(p_node->'args') AS a LOOP
            IF mandate.valid_condition_node(v_arg, p_depth + 1) IS NOT TRUE THEN
                RETURN false;
            END IF;
        END LOOP;
        RETURN true;
    ELSIF v_op IN ('==', '!=', '<', '<=', '>', '>=') THEN
        IF v_keys <> ARRAY['op', 'path', 'value'] THEN
            RETURN false;
        END IF;
        v_kind := jsonb_typeof(p_node->'value');
        RETURN COALESCE(mandate.valid_condition_path(p_node->'path')
            AND v_kind IN ('number', 'string', 'boolean')
            AND (v_kind <> 'boolean' OR v_op IN ('==', '!='))
            AND (p_node->'path'->>0 <> 'action' OR (v_kind = 'string' AND v_op IN ('==', '!='))), false);
    ELSIF v_op = 'in' THEN
        IF v_keys <> ARRAY['op', 'path', 'values'] OR NOT mandate.valid_condition_path(p_node->'path')
            OR jsonb_typeof(p_node->'values') IS DISTINCT FROM 'array' THEN
            RETURN false;
        END IF;
        IF jsonb_array_length(p_node->'values') NOT BETWEEN 1 AND 100 THEN
            RETURN false;
        END IF;
        v_kind := jsonb_typeof(p_node->'values'->0);
        RETURN COALESCE(v_kind IN ('number', 'string', 'boolean')
            AND (p_node->'path'->>0 <> 'action' OR v_kind = 'string')
            AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(p_node->'values') AS v WHERE jsonb_typeof(v) <> v_kind), false);
    ELSIF v_op = 'has' THEN
        RETURN COALESCE(v_keys = ARRAY['op', 'path'] AND mandate.valid_condition_path(p_node->'path')
            AND p_node->'path'->>0 = 'payload', false);
    END IF;
    RETURN false;
END
$$;

CREATE OR REPLACE FUNCTION mandate.valid_condition_plan(p_plan jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT COALESCE(octet_length(p_plan::text) <= 20000 AND mandate.valid_condition_node(p_plan, 1), false)
$$;

-- The plan of a live mandate is checked; a revoked mandate's is not, since
-- nothing evaluates it again. A mandate is never changed except to revoke
-- it, and it is never inserted revoked, so every plan is still checked when
-- it is written. NOT VALID leaves the rows that exist unread: one that fails
-- the stricter check can then still be revoked, which is all that is left
-- to do with it.
ALTER TABLE mandate.mandates DROP CONSTRAINT mandates_condition_plan_check;
ALTER TABLE mandate.mandates ADD CONSTRAINT mandates_condition_plan_check
    CHECK (condition_plan IS NULL OR revoked_at IS NOT NULL OR mandate.valid_condition_plan(condition_plan)) NOT VALID;
