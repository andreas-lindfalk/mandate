-- Restores the bodies of the three functions from 0001_init, and its
-- constraint.
CREATE OR REPLACE FUNCTION mandate.valid_condition_path(p_path jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT jsonb_typeof(p_path) = 'array'
        AND jsonb_array_length(p_path) BETWEEN 1 AND 11
        AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(p_path) AS s
            WHERE jsonb_typeof(s) IS DISTINCT FROM 'string' OR octet_length(s #>> '{}') NOT BETWEEN 1 AND 200)
        AND ((p_path->>0 = 'payload' AND jsonb_array_length(p_path) >= 2)
            OR (p_path->>0 = 'action' AND jsonb_array_length(p_path) = 1))
$$;

CREATE OR REPLACE FUNCTION mandate.valid_condition_node(p_node jsonb, p_depth int) RETURNS boolean
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

CREATE OR REPLACE FUNCTION mandate.valid_condition_plan(p_plan jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT octet_length(p_plan::text) <= 20000 AND mandate.valid_condition_node(p_plan, 1)
$$;

ALTER TABLE mandate.mandates DROP CONSTRAINT mandates_condition_plan_check;
ALTER TABLE mandate.mandates ADD CONSTRAINT mandates_condition_plan_check
    CHECK (condition_plan IS NULL OR mandate.valid_condition_plan(condition_plan)) NOT VALID;
