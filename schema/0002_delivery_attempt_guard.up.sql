-- An attempt opens only by counting it. 0001's guard compared only the
-- result, finished_at and settled_by of a delivery that stays pending, so
-- the system could set attempt_open_at without opening an attempt: past
-- the last moment one may open, which then let an answer be recorded as
-- final, or in the future, which kept the stale sweep and new attempts
-- away. Now only the opening branch sets it, to now(); every other change
-- leaves it as it was or closes it.
CREATE OR REPLACE FUNCTION mandate.deliveries_guard() RETURNS trigger
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
            WHERE p.id = NEW.proposal_id AND p.tenant_id = NEW.tenant_id AND p.status IN ('approved', 'executed') AND NOT p.dry_run
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
    -- Only that sets the time an attempt opened: anything else leaves it as
    -- it was or closes it, so an attempt never opens too late uncounted.
    IF NEW.attempt_open_at IS NOT NULL AND NEW.attempt_open_at IS DISTINCT FROM OLD.attempt_open_at THEN
        RAISE EXCEPTION 'an attempt opens only by counting it' USING ERRCODE = 'insufficient_privilege';
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
