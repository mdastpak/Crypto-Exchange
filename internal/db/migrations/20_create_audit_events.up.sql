-- Audit log (#51): who did what, when, from where, and with which outcome.
-- Rows are append-only: triggers refuse UPDATE, DELETE, and TRUNCATE to every
-- role, so only a schema change, which the migrations make, could remove the
-- protection.
--
-- Personal data is kept to what an audit needs: actors are identified by
-- their hk, never by a contact; the IP address and the user agent will be
-- encrypted with #140. The table only grows; its retention and partitioning
-- are part of #169.
CREATE TABLE audit_events (
    id          BIGSERIAL   PRIMARY KEY,
    -- The public identifier, a UUID v7, which also sorts the events by
    -- time, for the cursors of the admin query.
    hk          UUID        NOT NULL UNIQUE,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    actor_type  TEXT        NOT NULL CHECK (actor_type IN ('user', 'admin', 'anonymous', 'system')),
    -- The hk of the user or the administrator; none for anonymous and
    -- system actors.
    actor_id    UUID,
    action      TEXT        NOT NULL CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'),
    -- What the action applies to, such as a currency and its hk.
    target_type TEXT        CHECK (target_type ~ '^[a-z_]+$'),
    target_id   TEXT        CHECK (length(target_id) <= 64),
    outcome     TEXT        NOT NULL CHECK (outcome IN ('success', 'failure', 'denied')),
    -- Why an action failed or was denied, as a stable code.
    reason      TEXT        CHECK (reason ~ '^[a-z_]+$'),
    request_id  TEXT        CHECK (length(request_id) <= 64),
    ip          INET,
    user_agent  TEXT        CHECK (length(user_agent) <= 512),
    -- The fields that changed: {"before": {...}, "after": {...}}.
    changes     JSONB,
    CHECK ((actor_type IN ('user', 'admin')) = (actor_id IS NOT NULL)),
    CHECK ((target_type IS NULL) = (target_id IS NULL))
);

-- The admin query filters by actor, action, or target, newest first.
CREATE INDEX audit_events_actor ON audit_events (actor_id, hk) WHERE actor_id IS NOT NULL;
CREATE INDEX audit_events_action ON audit_events (action, hk);
CREATE INDEX audit_events_target ON audit_events (target_type, target_id, hk) WHERE target_id IS NOT NULL;

CREATE FUNCTION audit_events_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER audit_events_no_update_or_delete
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();

CREATE TRIGGER audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
