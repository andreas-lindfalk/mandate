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
ALTER TABLE mandate.proposals
    ADD COLUMN rationale  text CHECK (rationale IS NULL OR length(rationale) BETWEEN 1 AND 8000),
    ADD COLUMN evidence   jsonb NOT NULL DEFAULT '[]' CHECK (mandate.valid_evidence(evidence)),
    ADD COLUMN provenance jsonb NOT NULL DEFAULT '{}' CHECK (mandate.valid_provenance(provenance));

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
