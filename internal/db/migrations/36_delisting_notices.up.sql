-- The notices of delistings (#216): each stage that a delisting reaches,
-- the reminders before the freeze, and its cancel, told to the users. A
-- notice is a row of currency_delisting_notices, planned once by any
-- instance (the primary key makes a second plan do nothing), and fanned
-- out once into currency_delisting_notice_recipients: the users who hold
-- the currency, by every channel, and, but for a reminder, every other
-- user, by the free ones. Each recipient is sent once, at least, by the
-- instance that claims it with SKIP LOCKED. The rows stay, as the record
-- of who was told what.
CREATE TABLE currency_delisting_notices (
    delisting_hk uuid NOT NULL REFERENCES currency_delistings (hk),
    -- step:<state>, reminder:<days>d, or cancelled.
    kind text NOT NULL CHECK (kind ~ '^(step:[a-z_]+|reminder:[0-9]+d|cancelled)$'),
    -- The lifecycle state reached, or reminder, or cancelled.
    stage text NOT NULL CHECK (stage IN ('delist_announced', 'deposits_disabled', 'sell_only', 'trading_halted',
        'withdrawal_only', 'frozen', 'delisted', 'reminder', 'cancelled')),
    due_at timestamp with time zone NOT NULL,
    planned_at timestamp with time zone NOT NULL DEFAULT NOW(),
    -- When the recipients were written, and how many of them, and of those
    -- the holders. A notice that was stale by then, such as a step of a
    -- delisting cancelled since, has none.
    prepared_at timestamp with time zone,
    recipients integer CHECK (recipients >= 0),
    holders integer CHECK (holders >= 0 AND holders <= recipients),
    PRIMARY KEY (delisting_hk, kind),
    CHECK ((prepared_at IS NULL) = (recipients IS NULL) AND (recipients IS NULL) = (holders IS NULL))
);

CREATE INDEX currency_delisting_notices_unprepared ON currency_delisting_notices (due_at) WHERE prepared_at IS NULL;

CREATE TABLE currency_delisting_notice_recipients (
    delisting_hk uuid NOT NULL,
    kind text NOT NULL,
    user_hk uuid NOT NULL REFERENCES users (hk),
    -- Whether the user held the currency when the notice was fanned out:
    -- a holder is told by every channel, and first.
    holder boolean NOT NULL,
    -- The channels that the notice went out by, so that a retry does not
    -- send it twice by one.
    sent_channels text[] NOT NULL DEFAULT '{}',
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamp with time zone NOT NULL DEFAULT NOW(),
    -- sent: by at least one channel; no_contact: the user has no verified
    -- contact that a channel reaches; stale: the delisting was cancelled,
    -- or the account deleted, before it went; failed: it could not be sent
    -- after the last attempt.
    outcome text CHECK (outcome IN ('sent', 'no_contact', 'stale', 'failed')),
    done_at timestamp with time zone,
    PRIMARY KEY (delisting_hk, kind, user_hk),
    FOREIGN KEY (delisting_hk, kind) REFERENCES currency_delisting_notices (delisting_hk, kind),
    CHECK ((outcome IS NULL) = (done_at IS NULL))
);

-- The recipients that are left, holders first.
CREATE INDEX currency_delisting_notice_recipients_due ON currency_delisting_notice_recipients (holder DESC, next_attempt_at)
    WHERE done_at IS NULL;
