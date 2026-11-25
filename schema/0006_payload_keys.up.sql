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
