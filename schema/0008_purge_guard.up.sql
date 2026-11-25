-- The append-only guard let a DELETE through whenever the session setting
-- mandate.allow_purge was 'on'. Any role can set a custom setting, so the
-- only thing keeping the application role from deleting append-only rows
-- was that it holds no DELETE right on those tables. A grant added later,
-- by a host or by mistake, would have opened them.
--
-- Now a DELETE passes only when the setting is on and the statement runs as
-- the table's owner, which is what purge_tenant does: it is SECURITY
-- DEFINER, owned by the role that owns the schema, and not granted to the
-- application.
CREATE OR REPLACE FUNCTION mandate.forbid_change() RETURNS trigger
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
