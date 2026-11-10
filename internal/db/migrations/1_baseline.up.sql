-- The baseline of the schema: every table, type, function, trigger, and
-- index of the exchange, and the one row that the server expects to find.
-- It is in the form that pg_dump writes; every later change is a migration
-- of its own after this one.

CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

CREATE EXTENSION IF NOT EXISTS "uuid-ossp" WITH SCHEMA public;

CREATE TYPE public.account_status_type AS ENUM (
    'ACTIVE',
    'INACTIVE',
    'SUSPENDED',
    'BANNED',
    'PENDING_VERIFICATION'
);

CREATE TYPE public.aml_action AS ENUM (
    'ALERT',
    'REVIEW',
    'BLOCK',
    'FREEZE'
);

CREATE TYPE public.aml_alert_status AS ENUM (
    'OPEN',
    'INVESTIGATING',
    'RESOLVED',
    'DISMISSED',
    'ESCALATED'
);

CREATE TYPE public.aml_rule_type AS ENUM (
    'VELOCITY',
    'STRUCTURING',
    'HIGH_RISK_COUNTRY',
    'MIXER',
    'DARKNET',
    'SANCTIONS',
    'PEP',
    'UNUSUAL_PATTERN'
);

CREATE TYPE public.api_key_status AS ENUM (
    'ACTIVE',
    'INACTIVE',
    'REVOKED',
    'EXPIRED'
);

CREATE TYPE public.api_key_type AS ENUM (
    'HMAC',
    'ED25519'
);

CREATE TYPE public.asset_status AS ENUM (
    'ACTIVE',
    'INACTIVE',
    'DEPRECATED',
    'DELISTED'
);

CREATE TYPE public.asset_type AS ENUM (
    'FIAT',
    'COIN',
    'TOKEN',
    'STABLE',
    'WRAPPED'
);

CREATE TYPE public.auth_method AS ENUM (
    'PASSWORD',
    'API_KEY',
    'OAUTH',
    'MAGIC_LINK',
    'WEBAUTHN'
);

CREATE TYPE public.contact_type AS ENUM (
    'email',
    'mobile',
    'username'
);

CREATE TYPE public.custody_provider_type AS ENUM (
    'INTERNAL',
    'BTC',
    'EVM',
    'SOLANA',
    'FIAT'
);

CREATE TYPE public.deposit_status_type AS ENUM (
    'DETECTED',
    'CONFIRMING',
    'CONFIRMED',
    'CREDITED',
    'FAILED'
);

CREATE TYPE public.document_status AS ENUM (
    'PENDING',
    'APPROVED',
    'REJECTED',
    'EXPIRED'
);

CREATE TYPE public.document_type AS ENUM (
    'PASSPORT',
    'NATIONAL_ID',
    'DRIVERS_LICENSE',
    'UTILITY_BILL',
    'BANK_STATEMENT',
    'PROOF_OF_ADDRESS',
    'SELFIE',
    'SOURCE_OF_FUNDS',
    'CORPORATE_DOCUMENT'
);

CREATE DOMAIN public.exact_decimal AS numeric
	CONSTRAINT exact_decimal_check CHECK (((scale(VALUE) <= 36) AND (abs(VALUE) < '1000000000000000000000000000000000000000000'::numeric)));

CREATE TYPE public.fee_level AS ENUM (
    'SLOW',
    'NORMAL',
    'FAST',
    'CUSTOM'
);

CREATE TYPE public.kyc_status AS ENUM (
    'PENDING',
    'APPROVED',
    'REJECTED',
    'EXPIRED',
    'UNDER_REVIEW'
);

CREATE TYPE public.kyc_tier AS ENUM (
    'NONE',
    'BASIC',
    'STANDARD',
    'ENHANCED',
    'INSTITUTIONAL'
);

CREATE TYPE public.market_status AS ENUM (
    'ACTIVE',
    'INACTIVE',
    'MAINTENANCE',
    'SUSPENDED',
    'DELISTED'
);

CREATE TYPE public.order_side AS ENUM (
    'BUY',
    'SELL'
);

CREATE TYPE public.order_status AS ENUM (
    'NEW',
    'PARTIALLY_FILLED',
    'FILLED',
    'CANCELLED',
    'REJECTED',
    'EXPIRED'
);

CREATE TYPE public.order_type AS ENUM (
    'MARKET',
    'LIMIT',
    'STOP',
    'STOP_LIMIT'
);

CREATE TYPE public.sanction_action AS ENUM (
    'ALLOW',
    'REVIEW',
    'BLOCK'
);

CREATE TYPE public.screen_type AS ENUM (
    'USER_ONBOARDING',
    'DEPOSIT_ADDRESS',
    'WITHDRAWAL_ADDRESS',
    'COUNTERPARTY',
    'PERIODIC_REVIEW',
    'TRANSACTION'
);

CREATE TYPE public.session_status AS ENUM (
    'ACTIVE',
    'REVOKED',
    'EXPIRED'
);

CREATE TYPE public.settlement_mode AS ENUM (
    'OFF_CHAIN',
    'ON_CHAIN'
);

CREATE TYPE public.settlement_status AS ENUM (
    'PENDING',
    'PROCESSING',
    'COMPLETED',
    'FAILED',
    'CANCELLED'
);

CREATE TYPE public.settlement_type AS ENUM (
    'TRADE',
    'WITHDRAWAL',
    'DEPOSIT',
    'TRANSFER',
    'FEE',
    'FUNDING',
    'LIQUIDATION'
);

CREATE TYPE public.status_enum AS ENUM (
    'approved',
    'deleted',
    'pending',
    'suspend'
);

CREATE TYPE public.sub_account_status AS ENUM (
    'ACTIVE',
    'INACTIVE',
    'FROZEN'
);

CREATE TYPE public.time_in_force AS ENUM (
    'GTC',
    'IOC',
    'FOK',
    'GTX'
);

CREATE TYPE public.trade_side AS ENUM (
    'BUY',
    'SELL'
);

CREATE TYPE public.withdrawal_status_type AS ENUM (
    'PENDING',
    'BROADCAST',
    'CONFIRMING',
    'COMPLETED',
    'FAILED',
    'CANCELLED',
    'REJECTED'
);

CREATE FUNCTION public.audit_events_append_only() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE FUNCTION public.currency_symbols_follow() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    new_symbol TEXT := upper(NEW.info->>'symbol');
BEGIN
    IF TG_OP = 'UPDATE' AND upper(OLD.info->>'symbol') IS NOT DISTINCT FROM new_symbol THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        -- The symbol before stays reserved for this currency.
        UPDATE currency_symbols SET current = FALSE WHERE currency_hk = NEW.hk AND current;
    END IF;
    -- A currency can take back a symbol it holds; any other held symbol is
    -- refused. A released symbol is held by no one.
    INSERT INTO currency_symbols (symbol, currency_hk, current) VALUES (new_symbol, NEW.hk, TRUE)
    ON CONFLICT (symbol) WHERE released_at IS NULL
    DO UPDATE SET current = TRUE WHERE currency_symbols.currency_hk = EXCLUDED.currency_hk;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the symbol % is reserved by another currency', new_symbol
            USING ERRCODE = 'unique_violation', CONSTRAINT = 'currency_symbols_reserved';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION public.market_candles_drop_partitions(p_resolution text, p_before timestamp with time zone) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'public', 'pg_temp'
    AS $_$
DECLARE
    period  TEXT := market_candles_period(p_resolution);
    part    RECORD;
    lo      TIMESTAMP;
    dropped INTEGER := 0;
BEGIN
    IF period IS NULL OR p_before IS NULL THEN
        RAISE EXCEPTION 'market candles: no partitions of % before %', p_resolution, p_before;
    END IF;
    FOR part IN
        SELECT c.relname FROM pg_inherits i
        JOIN pg_class c ON c.oid = i.inhrelid
        JOIN pg_class p ON p.oid = i.inhparent
        WHERE p.relname = 'market_candles_' || p_resolution
            AND c.relname ~ ('^market_candles_' || p_resolution || CASE period
                WHEN 'year' THEN '_[0-9]{4}$' WHEN 'month' THEN '_[0-9]{4}_[0-9]{2}$' ELSE '_[0-9]{4}_[0-9]{2}_[0-9]{2}$' END)
    LOOP
        IF period = 'year' THEN
            lo := to_timestamp(right(part.relname, 4), 'YYYY')::timestamp;
        ELSIF period = 'month' THEN
            lo := to_timestamp(right(part.relname, 7), 'YYYY_MM')::timestamp;
        ELSE
            lo := to_timestamp(right(part.relname, 10), 'YYYY_MM_DD')::timestamp;
        END IF;
        IF ((lo + ('1 ' || period)::interval) AT TIME ZONE 'UTC') <= p_before THEN
            EXECUTE format('DROP TABLE %I', part.relname);
            dropped := dropped + 1;
        END IF;
    END LOOP;
    RETURN dropped;
END
$_$;

CREATE FUNCTION public.market_candles_ensure_partition(p_resolution text, p_at timestamp with time zone) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'public', 'pg_temp'
    AS $$
DECLARE
    period TEXT := market_candles_period(p_resolution);
    lo     TIMESTAMP;
    name   TEXT;
BEGIN
    IF period IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'market candles: no partition for % at %', p_resolution, p_at;
    END IF;
    lo := date_trunc(period, p_at AT TIME ZONE 'UTC');
    name := 'market_candles_' || p_resolution || '_' ||
        to_char(lo, CASE period WHEN 'year' THEN 'YYYY' WHEN 'month' THEN 'YYYY_MM' ELSE 'YYYY_MM_DD' END);
    PERFORM pg_advisory_xact_lock(hashtext(name));
    IF to_regclass(name) IS NULL THEN
        EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
            name, 'market_candles_' || p_resolution,
            (lo AT TIME ZONE 'UTC'), ((lo + ('1 ' || period)::interval) AT TIME ZONE 'UTC'));
    END IF;
    RETURN name;
END
$$;

CREATE FUNCTION public.market_candles_period(p_resolution text) RETURNS text
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT CASE p_resolution
        WHEN '1d' THEN 'year' WHEN '1h' THEN 'month' WHEN '15m' THEN 'month'
        WHEN '5m' THEN 'month' WHEN '1m' THEN 'day' END
$$;

CREATE FUNCTION public.update_updated_at_column() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$;

CREATE TABLE public.aml_alerts (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    rule_id uuid NOT NULL,
    user_id uuid NOT NULL,
    sub_account_id uuid,
    trigger_data jsonb NOT NULL,
    risk_score numeric(5,2) DEFAULT 0 NOT NULL,
    status public.aml_alert_status DEFAULT 'OPEN'::public.aml_alert_status NOT NULL,
    assigned_to uuid,
    resolved_at timestamp with time zone,
    resolved_by uuid,
    resolution text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.aml_rules (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    name character varying(100) NOT NULL,
    description text,
    rule_type public.aml_rule_type NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    threshold_amount public.exact_decimal,
    threshold_count integer,
    time_window_hours integer,
    action public.aml_action DEFAULT 'ALERT'::public.aml_action NOT NULL,
    severity character varying(20) DEFAULT 'MEDIUM'::character varying NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.api_keys (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    sub_account_id uuid NOT NULL,
    name character varying(100) NOT NULL,
    public_key character varying(64) NOT NULL,
    key_type public.api_key_type DEFAULT 'HMAC'::public.api_key_type NOT NULL,
    permissions jsonb DEFAULT '[]'::jsonb NOT NULL,
    ip_whitelist text[] DEFAULT '{}'::text[],
    rate_limit_rest integer DEFAULT 100 NOT NULL,
    rate_limit_ws integer DEFAULT 50 NOT NULL,
    rate_limit_trade integer DEFAULT 10 NOT NULL,
    status public.api_key_status DEFAULT 'ACTIVE'::public.api_key_status NOT NULL,
    last_used_at timestamp with time zone,
    last_used_ip inet,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    secret_sealed bytea,
    ed25519_public_key bytea,
    CONSTRAINT api_keys_verification_material CHECK ((((key_type = 'HMAC'::public.api_key_type) AND (secret_sealed IS NOT NULL) AND (ed25519_public_key IS NULL)) OR ((key_type = 'ED25519'::public.api_key_type) AND (ed25519_public_key IS NOT NULL) AND (octet_length(ed25519_public_key) = 32) AND (secret_sealed IS NULL))))
);

CREATE TABLE public.assets (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    symbol character varying(20) NOT NULL,
    name character varying(100) NOT NULL,
    type public.asset_type NOT NULL,
    status public.asset_status DEFAULT 'INACTIVE'::public.asset_status NOT NULL,
    decimals smallint DEFAULT 8 NOT NULL,
    network character varying(50),
    contract_address character varying(100),
    native_symbol character varying(20),
    description text,
    logo_url character varying(500),
    website character varying(500),
    explorer_url character varying(500),
    min_withdrawal public.exact_decimal DEFAULT 0 NOT NULL,
    max_withdrawal public.exact_decimal DEFAULT 0 NOT NULL,
    withdrawal_fee public.exact_decimal DEFAULT 0 NOT NULL,
    deposit_enabled boolean DEFAULT false NOT NULL,
    withdrawal_enabled boolean DEFAULT false NOT NULL,
    custody_mode public.settlement_mode DEFAULT 'OFF_CHAIN'::public.settlement_mode NOT NULL,
    requires_tag boolean DEFAULT false NOT NULL,
    sanctions_screened boolean DEFAULT false NOT NULL,
    last_screened_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT assets_decimals_range CHECK (((decimals >= 0) AND (decimals <= 36)))
);

CREATE TABLE public.audit_events (
    id bigint NOT NULL,
    hk uuid NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    actor_type text NOT NULL,
    actor_id uuid,
    action text NOT NULL,
    target_type text,
    target_id text,
    outcome text NOT NULL,
    reason text,
    request_id text,
    ip inet,
    user_agent text,
    changes jsonb,
    CONSTRAINT audit_events_action_check CHECK ((action ~ '^[a-z_]+(\.[a-z_]+)+$'::text)),
    CONSTRAINT audit_events_actor_type_check CHECK ((actor_type = ANY (ARRAY['user'::text, 'admin'::text, 'anonymous'::text, 'system'::text]))),
    CONSTRAINT audit_events_check CHECK (((actor_type = ANY (ARRAY['user'::text, 'admin'::text])) = (actor_id IS NOT NULL))),
    CONSTRAINT audit_events_check1 CHECK (((target_type IS NULL) = (target_id IS NULL))),
    CONSTRAINT audit_events_outcome_check CHECK ((outcome = ANY (ARRAY['success'::text, 'failure'::text, 'denied'::text]))),
    CONSTRAINT audit_events_reason_check CHECK ((reason ~ '^[a-z_]+$'::text)),
    CONSTRAINT audit_events_request_id_check CHECK ((length(request_id) <= 64)),
    CONSTRAINT audit_events_target_id_check CHECK ((length(target_id) <= 64)),
    CONSTRAINT audit_events_target_type_check CHECK ((target_type ~ '^[a-z_]+$'::text)),
    CONSTRAINT audit_events_user_agent_check CHECK ((length(user_agent) <= 512))
);

CREATE SEQUENCE public.audit_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.audit_events_id_seq OWNED BY public.audit_events.id;

CREATE TABLE public.balance_changes (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    balance_id uuid NOT NULL,
    user_id uuid NOT NULL,
    sub_account_id uuid,
    asset_id uuid NOT NULL,
    change_type character varying(50) NOT NULL,
    amount public.exact_decimal NOT NULL,
    old_available public.exact_decimal NOT NULL,
    new_available public.exact_decimal NOT NULL,
    old_locked public.exact_decimal NOT NULL,
    new_locked public.exact_decimal NOT NULL,
    old_pending public.exact_decimal NOT NULL,
    new_pending public.exact_decimal NOT NULL,
    reference_id uuid,
    reference_type character varying(50),
    "timestamp" timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT balance_changes_valid CHECK ((((amount)::numeric > (0)::numeric) AND ((old_available)::numeric >= (0)::numeric) AND ((new_available)::numeric >= (0)::numeric) AND ((old_locked)::numeric >= (0)::numeric) AND ((new_locked)::numeric >= (0)::numeric) AND ((old_pending)::numeric >= (0)::numeric) AND ((new_pending)::numeric >= (0)::numeric)))
);

CREATE TABLE public.balance_snapshots (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    balance_id uuid NOT NULL,
    user_id uuid NOT NULL,
    sub_account_id uuid,
    asset_id uuid NOT NULL,
    available public.exact_decimal NOT NULL,
    locked public.exact_decimal NOT NULL,
    pending_deposit public.exact_decimal NOT NULL,
    total public.exact_decimal NOT NULL,
    version integer NOT NULL,
    "timestamp" timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT balance_snapshots_non_negative CHECK ((((available)::numeric >= (0)::numeric) AND ((locked)::numeric >= (0)::numeric) AND ((pending_deposit)::numeric >= (0)::numeric) AND ((total)::numeric = ((available)::numeric + (locked)::numeric))))
);

CREATE TABLE public.balances (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    user_id uuid NOT NULL,
    sub_account_id uuid,
    asset_id uuid NOT NULL,
    available public.exact_decimal DEFAULT 0 NOT NULL,
    locked public.exact_decimal DEFAULT 0 NOT NULL,
    pending_deposit public.exact_decimal DEFAULT 0 NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    total public.exact_decimal GENERATED ALWAYS AS (((available)::numeric + (locked)::numeric)) STORED,
    CONSTRAINT balances_non_negative CHECK ((((available)::numeric >= (0)::numeric) AND ((locked)::numeric >= (0)::numeric) AND ((pending_deposit)::numeric >= (0)::numeric)))
);

CREATE TABLE public.currencies (
    id integer NOT NULL,
    hk uuid DEFAULT gen_random_uuid() NOT NULL,
    status public.status_enum DEFAULT 'pending'::public.status_enum,
    info jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    lifecycle text DEFAULT 'active'::text NOT NULL,
    lifecycle_changed_at timestamp with time zone,
    CONSTRAINT currencies_lifecycle_check CHECK ((lifecycle = ANY (ARRAY['active'::text, 'delist_announced'::text, 'deposits_disabled'::text, 'sell_only'::text, 'trading_halted'::text, 'withdrawal_only'::text, 'frozen'::text, 'delisted'::text]))),
    CONSTRAINT currencies_symbol_upper_case CHECK (((info ->> 'symbol'::text) = upper((info ->> 'symbol'::text)))),
    CONSTRAINT currencies_version_check CHECK ((version >= 1)),
    CONSTRAINT info_structure CHECK (((info ? 'fa_name'::text) AND (info ? 'en_name'::text) AND (info ? 'symbol'::text) AND (info ? 'if_fiat'::text) AND (info ? 'is_token'::text) AND (info ? 'contract'::text)))
);

CREATE SEQUENCE public.currencies_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.currencies_id_seq OWNED BY public.currencies.id;

CREATE TABLE public.currency_capability_overrides (
    currency_hk uuid NOT NULL,
    capability text NOT NULL,
    allowed boolean NOT NULL,
    note text,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    cleared_at timestamp with time zone,
    cleared_by uuid,
    CONSTRAINT currency_capability_overrides_capability_check CHECK ((capability = ANY (ARRAY['deposit'::text, 'withdraw'::text, 'buy'::text, 'sell'::text, 'convert_into'::text, 'convert_from'::text, 'listed'::text]))),
    CONSTRAINT currency_capability_overrides_cleared CHECK (((cleared_at IS NULL) = (cleared_by IS NULL))),
    CONSTRAINT currency_capability_overrides_note_check CHECK ((length(note) <= 1000))
);

CREATE TABLE public.currency_delisting_steps (
    delisting_hk uuid NOT NULL,
    state text NOT NULL,
    scheduled_at timestamp with time zone NOT NULL,
    applied_at timestamp with time zone,
    CONSTRAINT currency_delisting_steps_state_check CHECK ((state = ANY (ARRAY['delist_announced'::text, 'deposits_disabled'::text, 'sell_only'::text, 'trading_halted'::text, 'withdrawal_only'::text, 'frozen'::text, 'delisted'::text])))
);

CREATE TABLE public.currency_delistings (
    hk uuid NOT NULL,
    currency_hk uuid NOT NULL,
    kind text NOT NULL,
    status text DEFAULT 'scheduled'::text NOT NULL,
    settlement text NOT NULL,
    settlement_currency_hk uuid,
    price_source text,
    fixed_price public.exact_decimal,
    claim_days integer,
    announcement_url text,
    note text,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    cancelled_at timestamp with time zone,
    cancelled_by uuid,
    completed_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT currency_delistings_announcement_url_check CHECK ((length(announcement_url) <= 2048)),
    CONSTRAINT currency_delistings_check CHECK (((settlement = 'convert'::text) = ((settlement_currency_hk IS NOT NULL) AND (price_source IS NOT NULL)))),
    CONSTRAINT currency_delistings_check1 CHECK (((settlement = 'claim'::text) = (claim_days IS NOT NULL))),
    CONSTRAINT currency_delistings_check2 CHECK (((price_source = 'fixed'::text) = (fixed_price IS NOT NULL))),
    CONSTRAINT currency_delistings_check3 CHECK ((settlement_currency_hk IS DISTINCT FROM currency_hk)),
    CONSTRAINT currency_delistings_check4 CHECK (((status = 'cancelled'::text) = ((cancelled_at IS NOT NULL) AND (cancelled_by IS NOT NULL)))),
    CONSTRAINT currency_delistings_check5 CHECK (((status = 'completed'::text) = (completed_at IS NOT NULL))),
    CONSTRAINT currency_delistings_claim_days_check CHECK (((claim_days >= 1) AND (claim_days <= 3650))),
    CONSTRAINT currency_delistings_fixed_price_check CHECK (((fixed_price)::numeric > (0)::numeric)),
    CONSTRAINT currency_delistings_kind_check CHECK ((kind = ANY (ARRAY['standard'::text, 'emergency'::text]))),
    CONSTRAINT currency_delistings_note_check CHECK ((length(note) <= 1000)),
    CONSTRAINT currency_delistings_price_source_check CHECK ((price_source = ANY (ARRAY['last_trade'::text, 'twap'::text, 'fixed'::text]))),
    CONSTRAINT currency_delistings_settlement_check CHECK ((settlement = ANY (ARRAY['convert'::text, 'claim'::text]))),
    CONSTRAINT currency_delistings_status_check CHECK ((status = ANY (ARRAY['scheduled'::text, 'completed'::text, 'cancelled'::text])))
);

CREATE TABLE public.currency_migration_phases (
    migration_hk uuid NOT NULL,
    phase text NOT NULL,
    scheduled_at timestamp with time zone NOT NULL,
    applied_at timestamp with time zone,
    CONSTRAINT currency_migration_phases_phase_check CHECK ((phase = ANY (ARRAY['announced'::text, 'deposits_paused'::text, 'trading_halted'::text, 'converting'::text, 'completed'::text])))
);

CREATE TABLE public.currency_migration_sources (
    migration_hk uuid NOT NULL,
    currency_hk uuid NOT NULL,
    ratio public.exact_decimal NOT NULL,
    CONSTRAINT currency_migration_sources_ratio_check CHECK (((ratio)::numeric > (0)::numeric))
);

CREATE TABLE public.currency_migrations (
    hk uuid NOT NULL,
    target_hk uuid NOT NULL,
    forced boolean NOT NULL,
    phase text DEFAULT 'scheduled'::text NOT NULL,
    announcement_url text,
    note text,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    cancelled_at timestamp with time zone,
    cancelled_by uuid,
    completed_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT currency_migrations_announcement_url_check CHECK ((length(announcement_url) <= 2048)),
    CONSTRAINT currency_migrations_check CHECK (((phase = 'cancelled'::text) = ((cancelled_at IS NOT NULL) AND (cancelled_by IS NOT NULL)))),
    CONSTRAINT currency_migrations_check1 CHECK (((phase = 'completed'::text) = (completed_at IS NOT NULL))),
    CONSTRAINT currency_migrations_note_check CHECK ((length(note) <= 1000)),
    CONSTRAINT currency_migrations_phase_check CHECK ((phase = ANY (ARRAY['scheduled'::text, 'announced'::text, 'deposits_paused'::text, 'trading_halted'::text, 'converting'::text, 'completed'::text, 'cancelled'::text])))
);

CREATE TABLE public.currency_symbols (
    symbol text NOT NULL,
    currency_hk uuid NOT NULL,
    current boolean NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    id bigint NOT NULL,
    released_at timestamp with time zone,
    released_by uuid,
    CONSTRAINT currency_symbols_released CHECK (((released_at IS NULL) = (released_by IS NULL))),
    CONSTRAINT currency_symbols_released_not_current CHECK (((released_at IS NULL) OR (NOT current)))
);

ALTER TABLE public.currency_symbols ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.currency_symbols_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.custody_configs (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    provider_type public.custody_provider_type NOT NULL,
    asset_id uuid NOT NULL,
    network character varying(50) NOT NULL,
    rpc_endpoints text[] DEFAULT '{}'::text[] NOT NULL,
    ws_endpoints text[] DEFAULT '{}'::text[],
    explorer_api character varying(500),
    explorer_ws character varying(500),
    api_key character varying(200),
    api_secret character varying(500),
    jwt_token text,
    chain_id character varying(50),
    contract_address character varying(100),
    decimals smallint DEFAULT 18 NOT NULL,
    confirmations_required integer DEFAULT 1 NOT NULL,
    min_deposit_amount public.exact_decimal DEFAULT 0 NOT NULL,
    min_withdrawal_amount public.exact_decimal DEFAULT 0 NOT NULL,
    max_withdrawal_amount public.exact_decimal DEFAULT 0 NOT NULL,
    default_fee_level public.fee_level DEFAULT 'NORMAL'::public.fee_level NOT NULL,
    fee_asset_id uuid,
    hot_wallet_address character varying(200),
    cold_wallet_address character varying(200),
    block_poll_interval integer DEFAULT 30 NOT NULL,
    reorg_depth integer DEFAULT 1 NOT NULL,
    sanctions_screening boolean DEFAULT true NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT custody_configs_decimals_range CHECK (((decimals >= 0) AND (decimals <= 36)))
);

CREATE TABLE public.data_keys (
    purpose text NOT NULL,
    version integer NOT NULL,
    kek_id text NOT NULL,
    wrapped_key bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_at timestamp with time zone,
    active_from timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT data_keys_purpose_check CHECK ((purpose = ANY (ARRAY['row-mac'::text, 'field-enc'::text, 'blind-index'::text, 'otp-mac'::text]))),
    CONSTRAINT data_keys_version_check CHECK ((version > 0))
);

CREATE TABLE public.deposit_addresses (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    user_id uuid NOT NULL,
    sub_account_id uuid,
    asset_id uuid NOT NULL,
    address character varying(200) NOT NULL,
    tag character varying(100),
    derivation_path character varying(100),
    index integer,
    active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.deposits (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    tx_hash character varying(200) NOT NULL,
    asset_id uuid NOT NULL,
    address character varying(200) NOT NULL,
    tag character varying(100),
    amount public.exact_decimal NOT NULL,
    confirmed_amount public.exact_decimal DEFAULT 0 NOT NULL,
    confirmations integer DEFAULT 0 NOT NULL,
    required_confirms integer DEFAULT 1 NOT NULL,
    status public.deposit_status_type DEFAULT 'DETECTED'::public.deposit_status_type NOT NULL,
    detected_at timestamp with time zone DEFAULT now() NOT NULL,
    confirmed_at timestamp with time zone,
    credited_at timestamp with time zone,
    block_height bigint,
    block_hash character varying(100),
    raw_data jsonb
);

CREATE TABLE public.integrity_findings (
    id bigint NOT NULL,
    table_name text NOT NULL,
    row_key text NOT NULL,
    check_failed text NOT NULL,
    detected_by text NOT NULL,
    detected_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    seen_count bigint DEFAULT 1 NOT NULL,
    resolved_at timestamp with time zone,
    resolution text,
    resolved_by uuid,
    reason text,
    CONSTRAINT integrity_findings_check CHECK (((resolved_at IS NULL) = (resolution IS NULL))),
    CONSTRAINT integrity_findings_check_failed_check CHECK ((check_failed = ANY (ARRAY['missing'::text, 'mismatch'::text, 'unknown_key'::text]))),
    CONSTRAINT integrity_findings_detected_by_check CHECK ((detected_by = ANY (ARRAY['read'::text, 'verifier'::text]))),
    CONSTRAINT integrity_findings_reason_check CHECK ((length(reason) <= 1000)),
    CONSTRAINT integrity_findings_resolution_check CHECK ((resolution = ANY (ARRAY['resealed'::text, 'report_mode'::text, 'row_gone'::text]))),
    CONSTRAINT integrity_findings_table_name_check CHECK ((table_name = ANY (ARRAY['users'::text, 'user_contacts'::text, 'sessions'::text])))
);

CREATE SEQUENCE public.integrity_findings_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.integrity_findings_id_seq OWNED BY public.integrity_findings.id;

CREATE TABLE public.kyc_applications (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    user_id uuid NOT NULL,
    requested_tier public.kyc_tier NOT NULL,
    status public.kyc_status DEFAULT 'PENDING'::public.kyc_status NOT NULL,
    form_data_enc text,
    documents uuid[] DEFAULT '{}'::uuid[],
    reviewed_at timestamp with time zone,
    reviewed_by uuid,
    review_notes text,
    provider character varying(50),
    provider_ref_id character varying(100),
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.kyc_documents (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    kyc_profile_id uuid NOT NULL,
    type public.document_type NOT NULL,
    status public.document_status DEFAULT 'PENDING'::public.document_status NOT NULL,
    file_name character varying(255) NOT NULL,
    file_size bigint NOT NULL,
    mime_type character varying(100) NOT NULL,
    storage_path character varying(500) NOT NULL,
    verified_at timestamp with time zone,
    verified_by uuid,
    reject_reason text,
    extracted_data_enc text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.kyc_profiles (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    user_id uuid NOT NULL,
    status public.kyc_status DEFAULT 'PENDING'::public.kyc_status NOT NULL,
    tier public.kyc_tier DEFAULT 'NONE'::public.kyc_tier NOT NULL,
    first_name_enc text,
    last_name_enc text,
    date_of_birth_enc date,
    nationality_enc character varying(100),
    country_of_residence_enc character varying(100),
    address_line1_enc text,
    address_line2_enc text,
    city_enc character varying(100),
    state_enc character varying(100),
    postal_code_enc character varying(20),
    country_enc character varying(100),
    verified_at timestamp with time zone,
    verified_by uuid,
    expires_at timestamp with time zone,
    risk_score smallint DEFAULT 0 NOT NULL,
    risk_factors text[] DEFAULT '{}'::text[],
    provider character varying(50),
    provider_ref_id character varying(100),
    admin_notes text,
    reject_reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.market_asset_contracts (
    network text NOT NULL,
    address text NOT NULL,
    asset_id bigint NOT NULL,
    source text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_asset_contracts_address_check CHECK (((length(address) >= 1) AND (length(address) <= 200))),
    CONSTRAINT market_asset_contracts_network_check CHECK ((network ~ '^[a-z0-9_-]{1,60}$'::text))
);

CREATE TABLE public.market_asset_sources (
    source text NOT NULL,
    external_id text NOT NULL,
    asset_id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    details_fetched_at timestamp with time zone,
    CONSTRAINT market_asset_sources_external_id_check CHECK (((length(external_id) >= 1) AND (length(external_id) <= 200))),
    CONSTRAINT market_asset_sources_source_check CHECK ((source ~ '^[a-z0-9_]{1,40}$'::text))
);

CREATE TABLE public.market_assets (
    id bigint NOT NULL,
    slug text NOT NULL,
    symbol text NOT NULL,
    name text NOT NULL,
    kind text DEFAULT 'unknown'::text NOT NULL,
    status text DEFAULT 'unverified'::text NOT NULL,
    currency_hk uuid,
    active boolean DEFAULT true NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    links jsonb DEFAULT '{}'::jsonb NOT NULL,
    logo_url text DEFAULT ''::text NOT NULL,
    market_cap_rank integer,
    circulating_supply numeric,
    total_supply numeric,
    max_supply numeric,
    attribution text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    tier_override text,
    CONSTRAINT market_assets_circulating_supply_check CHECK ((circulating_supply >= (0)::numeric)),
    CONSTRAINT market_assets_description_check CHECK ((length(description) <= 20000)),
    CONSTRAINT market_assets_kind_check CHECK ((kind = ANY (ARRAY['coin'::text, 'token'::text, 'unknown'::text]))),
    CONSTRAINT market_assets_link_check CHECK (((status = 'linked'::text) = (currency_hk IS NOT NULL))),
    CONSTRAINT market_assets_links_check CHECK ((jsonb_typeof(links) = 'object'::text)),
    CONSTRAINT market_assets_logo_url_check CHECK (((logo_url = ''::text) OR (logo_url ~~ 'https://%'::text))),
    CONSTRAINT market_assets_market_cap_rank_check CHECK ((market_cap_rank > 0)),
    CONSTRAINT market_assets_max_supply_check CHECK ((max_supply >= (0)::numeric)),
    CONSTRAINT market_assets_name_check CHECK (((length(name) >= 1) AND (length(name) <= 200))),
    CONSTRAINT market_assets_slug_check CHECK (((slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'::text) AND (length(slug) <= 100))),
    CONSTRAINT market_assets_status_check CHECK ((status = ANY (ARRAY['unverified'::text, 'hidden'::text, 'linked'::text]))),
    CONSTRAINT market_assets_symbol_check CHECK (((symbol = upper(symbol)) AND ((length(symbol) >= 1) AND (length(symbol) <= 32)))),
    CONSTRAINT market_assets_tier_override_check CHECK ((tier_override = ANY (ARRAY['hot'::text, 'active'::text, 'other'::text]))),
    CONSTRAINT market_assets_total_supply_check CHECK ((total_supply >= (0)::numeric))
);

ALTER TABLE public.market_assets ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.market_assets_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.market_candle_cursors (
    pair_id bigint NOT NULL,
    resolution text NOT NULL,
    from_time timestamp with time zone NOT NULL,
    until_time timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_candle_cursors_range_check CHECK ((from_time <= until_time)),
    CONSTRAINT market_candle_cursors_resolution_check CHECK ((resolution = ANY (ARRAY['1m'::text, '5m'::text, '15m'::text, '1h'::text, '1d'::text])))
);

CREATE TABLE public.market_candles (
    pair_id bigint NOT NULL,
    resolution text NOT NULL,
    open_time timestamp with time zone NOT NULL,
    open numeric NOT NULL,
    high numeric NOT NULL,
    low numeric NOT NULL,
    close numeric NOT NULL,
    volume numeric NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_candles_close_check CHECK ((close >= (0)::numeric)),
    CONSTRAINT market_candles_high_check CHECK ((high >= (0)::numeric)),
    CONSTRAINT market_candles_low_check CHECK ((low >= (0)::numeric)),
    CONSTRAINT market_candles_open_check CHECK ((open >= (0)::numeric)),
    CONSTRAINT market_candles_range_check CHECK ((low <= high)),
    CONSTRAINT market_candles_resolution_check CHECK ((resolution = ANY (ARRAY['1m'::text, '5m'::text, '15m'::text, '1h'::text, '1d'::text]))),
    CONSTRAINT market_candles_volume_check CHECK ((volume >= (0)::numeric))
)
PARTITION BY LIST (resolution);

CREATE TABLE public.market_candles_15m (
    pair_id bigint NOT NULL,
    resolution text NOT NULL,
    open_time timestamp with time zone NOT NULL,
    open numeric NOT NULL,
    high numeric NOT NULL,
    low numeric NOT NULL,
    close numeric NOT NULL,
    volume numeric NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_candles_close_check CHECK ((close >= (0)::numeric)),
    CONSTRAINT market_candles_high_check CHECK ((high >= (0)::numeric)),
    CONSTRAINT market_candles_low_check CHECK ((low >= (0)::numeric)),
    CONSTRAINT market_candles_open_check CHECK ((open >= (0)::numeric)),
    CONSTRAINT market_candles_range_check CHECK ((low <= high)),
    CONSTRAINT market_candles_resolution_check CHECK ((resolution = ANY (ARRAY['1m'::text, '5m'::text, '15m'::text, '1h'::text, '1d'::text]))),
    CONSTRAINT market_candles_volume_check CHECK ((volume >= (0)::numeric))
)
PARTITION BY RANGE (open_time);

CREATE TABLE public.market_candles_1d (
    pair_id bigint NOT NULL,
    resolution text NOT NULL,
    open_time timestamp with time zone NOT NULL,
    open numeric NOT NULL,
    high numeric NOT NULL,
    low numeric NOT NULL,
    close numeric NOT NULL,
    volume numeric NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_candles_close_check CHECK ((close >= (0)::numeric)),
    CONSTRAINT market_candles_high_check CHECK ((high >= (0)::numeric)),
    CONSTRAINT market_candles_low_check CHECK ((low >= (0)::numeric)),
    CONSTRAINT market_candles_open_check CHECK ((open >= (0)::numeric)),
    CONSTRAINT market_candles_range_check CHECK ((low <= high)),
    CONSTRAINT market_candles_resolution_check CHECK ((resolution = ANY (ARRAY['1m'::text, '5m'::text, '15m'::text, '1h'::text, '1d'::text]))),
    CONSTRAINT market_candles_volume_check CHECK ((volume >= (0)::numeric))
)
PARTITION BY RANGE (open_time);

CREATE TABLE public.market_candles_1h (
    pair_id bigint NOT NULL,
    resolution text NOT NULL,
    open_time timestamp with time zone NOT NULL,
    open numeric NOT NULL,
    high numeric NOT NULL,
    low numeric NOT NULL,
    close numeric NOT NULL,
    volume numeric NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_candles_close_check CHECK ((close >= (0)::numeric)),
    CONSTRAINT market_candles_high_check CHECK ((high >= (0)::numeric)),
    CONSTRAINT market_candles_low_check CHECK ((low >= (0)::numeric)),
    CONSTRAINT market_candles_open_check CHECK ((open >= (0)::numeric)),
    CONSTRAINT market_candles_range_check CHECK ((low <= high)),
    CONSTRAINT market_candles_resolution_check CHECK ((resolution = ANY (ARRAY['1m'::text, '5m'::text, '15m'::text, '1h'::text, '1d'::text]))),
    CONSTRAINT market_candles_volume_check CHECK ((volume >= (0)::numeric))
)
PARTITION BY RANGE (open_time);

CREATE TABLE public.market_candles_1m (
    pair_id bigint NOT NULL,
    resolution text NOT NULL,
    open_time timestamp with time zone NOT NULL,
    open numeric NOT NULL,
    high numeric NOT NULL,
    low numeric NOT NULL,
    close numeric NOT NULL,
    volume numeric NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_candles_close_check CHECK ((close >= (0)::numeric)),
    CONSTRAINT market_candles_high_check CHECK ((high >= (0)::numeric)),
    CONSTRAINT market_candles_low_check CHECK ((low >= (0)::numeric)),
    CONSTRAINT market_candles_open_check CHECK ((open >= (0)::numeric)),
    CONSTRAINT market_candles_range_check CHECK ((low <= high)),
    CONSTRAINT market_candles_resolution_check CHECK ((resolution = ANY (ARRAY['1m'::text, '5m'::text, '15m'::text, '1h'::text, '1d'::text]))),
    CONSTRAINT market_candles_volume_check CHECK ((volume >= (0)::numeric))
)
PARTITION BY RANGE (open_time);

CREATE TABLE public.market_candles_5m (
    pair_id bigint NOT NULL,
    resolution text NOT NULL,
    open_time timestamp with time zone NOT NULL,
    open numeric NOT NULL,
    high numeric NOT NULL,
    low numeric NOT NULL,
    close numeric NOT NULL,
    volume numeric NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_candles_close_check CHECK ((close >= (0)::numeric)),
    CONSTRAINT market_candles_high_check CHECK ((high >= (0)::numeric)),
    CONSTRAINT market_candles_low_check CHECK ((low >= (0)::numeric)),
    CONSTRAINT market_candles_open_check CHECK ((open >= (0)::numeric)),
    CONSTRAINT market_candles_range_check CHECK ((low <= high)),
    CONSTRAINT market_candles_resolution_check CHECK ((resolution = ANY (ARRAY['1m'::text, '5m'::text, '15m'::text, '1h'::text, '1d'::text]))),
    CONSTRAINT market_candles_volume_check CHECK ((volume >= (0)::numeric))
)
PARTITION BY RANGE (open_time);

CREATE TABLE public.market_identity_conflicts (
    id bigint NOT NULL,
    source text NOT NULL,
    external_id text NOT NULL,
    kind text NOT NULL,
    subject text DEFAULT ''::text NOT NULL,
    asset_id bigint,
    claimed_by bigint,
    detail text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    resolved_at timestamp with time zone,
    resolved_note text DEFAULT ''::text NOT NULL,
    CONSTRAINT market_identity_conflicts_detail_check CHECK ((length(detail) <= 1000)),
    CONSTRAINT market_identity_conflicts_kind_check CHECK ((kind = ANY (ARRAY['contract'::text, 'identity'::text, 'symbol'::text])))
);

ALTER TABLE public.market_identity_conflicts ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.market_identity_conflicts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.market_job_leases (
    job text NOT NULL,
    holder text DEFAULT ''::text NOT NULL,
    token bigint DEFAULT 0 NOT NULL,
    lease_until timestamp with time zone DEFAULT '-infinity'::timestamp with time zone NOT NULL,
    next_run_at timestamp with time zone DEFAULT now() NOT NULL,
    failures integer DEFAULT 0 NOT NULL,
    last_started_at timestamp with time zone,
    last_finished_at timestamp with time zone,
    last_success_at timestamp with time zone,
    CONSTRAINT market_job_leases_failures_check CHECK ((failures >= 0)),
    CONSTRAINT market_job_leases_job_check CHECK ((job ~ '^[a-z0-9_.:-]{1,100}$'::text))
);

CREATE TABLE public.market_source_controls (
    source text NOT NULL,
    paused boolean DEFAULT false NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_source_controls_reason_check CHECK ((length(reason) <= 1000)),
    CONSTRAINT market_source_controls_source_check CHECK ((source ~ '^[a-z0-9_]{1,40}$'::text))
);

CREATE TABLE public.market_source_usage (
    source text NOT NULL,
    window_kind text NOT NULL,
    window_start timestamp with time zone NOT NULL,
    requests integer NOT NULL,
    CONSTRAINT market_source_usage_requests_check CHECK ((requests >= 0)),
    CONSTRAINT market_source_usage_source_check CHECK ((source ~ '^[a-z0-9_]{1,40}$'::text)),
    CONSTRAINT market_source_usage_window_kind_check CHECK ((window_kind = ANY (ARRAY['minute'::text, 'month'::text])))
);

CREATE TABLE public.market_stats_24h (
    market_id uuid NOT NULL,
    "timestamp" timestamp with time zone NOT NULL,
    last_price public.exact_decimal,
    open_price public.exact_decimal,
    high_price public.exact_decimal,
    low_price public.exact_decimal,
    price_change public.exact_decimal,
    price_change_pct public.exact_decimal,
    base_volume public.exact_decimal,
    quote_volume public.exact_decimal,
    trade_count bigint,
    bid_price public.exact_decimal,
    bid_size public.exact_decimal,
    ask_price public.exact_decimal,
    ask_size public.exact_decimal,
    spread public.exact_decimal,
    spread_bps integer,
    funding_rate public.exact_decimal,
    next_funding_time timestamp with time zone
);

CREATE TABLE public.market_sync_runs (
    id bigint NOT NULL,
    job text NOT NULL,
    node text NOT NULL,
    mode text NOT NULL,
    status text DEFAULT 'running'::text NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    items_read integer DEFAULT 0 NOT NULL,
    items_written integer DEFAULT 0 NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    CONSTRAINT market_sync_runs_error_check CHECK ((length(error) <= 4000)),
    CONSTRAINT market_sync_runs_finished_check CHECK (((status = 'running'::text) = (finished_at IS NULL))),
    CONSTRAINT market_sync_runs_items_read_check CHECK ((items_read >= 0)),
    CONSTRAINT market_sync_runs_items_written_check CHECK ((items_written >= 0)),
    CONSTRAINT market_sync_runs_job_check CHECK ((job ~ '^[a-z0-9_.:-]{1,100}$'::text)),
    CONSTRAINT market_sync_runs_mode_check CHECK ((mode = ANY (ARRAY['live'::text, 'dry_run'::text]))),
    CONSTRAINT market_sync_runs_status_check CHECK ((status = ANY (ARRAY['running'::text, 'succeeded'::text, 'failed'::text])))
);

ALTER TABLE public.market_sync_runs ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.market_sync_runs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.market_tickers_latest (
    pair_id bigint NOT NULL,
    last_price numeric NOT NULL,
    bid numeric,
    ask numeric,
    volume_base numeric,
    volume_quote numeric,
    high numeric,
    low numeric,
    open_price numeric,
    observed_at timestamp with time zone NOT NULL,
    fetched_at timestamp with time zone NOT NULL,
    CONSTRAINT market_tickers_latest_ask_check CHECK ((ask >= (0)::numeric)),
    CONSTRAINT market_tickers_latest_bid_check CHECK ((bid >= (0)::numeric)),
    CONSTRAINT market_tickers_latest_high_check CHECK ((high >= (0)::numeric)),
    CONSTRAINT market_tickers_latest_last_price_check CHECK ((last_price >= (0)::numeric)),
    CONSTRAINT market_tickers_latest_low_check CHECK ((low >= (0)::numeric)),
    CONSTRAINT market_tickers_latest_open_price_check CHECK ((open_price >= (0)::numeric)),
    CONSTRAINT market_tickers_latest_volume_base_check CHECK ((volume_base >= (0)::numeric)),
    CONSTRAINT market_tickers_latest_volume_quote_check CHECK ((volume_quote >= (0)::numeric))
);

CREATE TABLE public.market_venue_pairs (
    id bigint NOT NULL,
    venue_id text NOT NULL,
    venue_symbol text NOT NULL,
    base_symbol text NOT NULL,
    quote_symbol text NOT NULL,
    base_asset_id bigint,
    quote_asset_id bigint,
    active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    tier text DEFAULT 'other'::text NOT NULL,
    tier_override text,
    tier_changed_at timestamp with time zone,
    CONSTRAINT market_venue_pairs_base_symbol_check CHECK (((base_symbol = upper(base_symbol)) AND ((length(base_symbol) >= 1) AND (length(base_symbol) <= 32)))),
    CONSTRAINT market_venue_pairs_quote_symbol_check CHECK (((quote_symbol = upper(quote_symbol)) AND ((length(quote_symbol) >= 1) AND (length(quote_symbol) <= 32)))),
    CONSTRAINT market_venue_pairs_tier_check CHECK ((tier = ANY (ARRAY['hot'::text, 'active'::text, 'other'::text]))),
    CONSTRAINT market_venue_pairs_tier_override_check CHECK ((tier_override = ANY (ARRAY['hot'::text, 'active'::text, 'other'::text]))),
    CONSTRAINT market_venue_pairs_venue_symbol_check CHECK (((length(venue_symbol) >= 1) AND (length(venue_symbol) <= 60)))
);

ALTER TABLE public.market_venue_pairs ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.market_venue_pairs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.market_venues (
    id text NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    region text NOT NULL,
    country text DEFAULT ''::text NOT NULL,
    website text DEFAULT ''::text NOT NULL,
    attribution text DEFAULT ''::text NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT market_venues_country_check CHECK (((country = ''::text) OR (country ~ '^[A-Z]{2}$'::text))),
    CONSTRAINT market_venues_id_check CHECK ((id ~ '^[a-z0-9_]{1,40}$'::text)),
    CONSTRAINT market_venues_kind_check CHECK ((kind = ANY (ARRAY['cex'::text, 'dex'::text]))),
    CONSTRAINT market_venues_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100))),
    CONSTRAINT market_venues_region_check CHECK ((region = ANY (ARRAY['global'::text, 'ir'::text]))),
    CONSTRAINT market_venues_website_check CHECK (((website = ''::text) OR (website ~~ 'https://%'::text)))
);

CREATE TABLE public.markets (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    base_asset_id uuid NOT NULL,
    quote_asset_id uuid NOT NULL,
    status public.market_status DEFAULT 'INACTIVE'::public.market_status NOT NULL,
    min_order_size public.exact_decimal DEFAULT 0 NOT NULL,
    max_order_size public.exact_decimal DEFAULT 0 NOT NULL,
    min_notional public.exact_decimal DEFAULT 0 NOT NULL,
    tick_size public.exact_decimal DEFAULT 0.01 NOT NULL,
    lot_size public.exact_decimal DEFAULT 0.001 NOT NULL,
    maker_fee_bps smallint DEFAULT 10 NOT NULL,
    taker_fee_bps smallint DEFAULT 20 NOT NULL,
    max_position_size public.exact_decimal DEFAULT 0 NOT NULL,
    max_leverage smallint DEFAULT 1 NOT NULL,
    market_maker_fee_bps smallint DEFAULT 0 NOT NULL,
    price_band_bps smallint DEFAULT 1000 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT chk_different_assets CHECK ((base_asset_id <> quote_asset_id))
);

CREATE TABLE public.order_events (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    order_id uuid NOT NULL,
    event_type character varying(50) NOT NULL,
    "timestamp" timestamp with time zone DEFAULT now() NOT NULL,
    old_status public.order_status,
    new_status public.order_status,
    old_filled public.exact_decimal,
    new_filled public.exact_decimal,
    trade_id uuid,
    trade_price public.exact_decimal,
    trade_qty public.exact_decimal,
    trade_fee public.exact_decimal,
    metadata jsonb
);

CREATE TABLE public.orders (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    client_order_id character varying(64),
    user_id uuid NOT NULL,
    sub_account_id uuid,
    market_id uuid NOT NULL,
    side public.order_side NOT NULL,
    type public.order_type NOT NULL,
    time_in_force public.time_in_force DEFAULT 'GTC'::public.time_in_force NOT NULL,
    price public.exact_decimal DEFAULT 0 NOT NULL,
    stop_price public.exact_decimal,
    quantity public.exact_decimal NOT NULL,
    filled_quantity public.exact_decimal DEFAULT 0 NOT NULL,
    status public.order_status DEFAULT 'NEW'::public.order_status NOT NULL,
    reject_reason text,
    fee_asset_id uuid NOT NULL,
    fee_paid public.exact_decimal DEFAULT 0 NOT NULL,
    fee_rate_bps smallint DEFAULT 0 NOT NULL,
    reduce_only boolean DEFAULT false NOT NULL,
    post_only boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    expired_at timestamp with time zone,
    cancelled_at timestamp with time zone
);

CREATE TABLE public.outbox_events (
    id bigint NOT NULL,
    topic text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error text,
    published_at timestamp with time zone,
    failed_at timestamp with time zone,
    CONSTRAINT outbox_events_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT outbox_events_topic_check CHECK ((topic <> ''::text))
);

CREATE SEQUENCE public.outbox_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.outbox_events_id_seq OWNED BY public.outbox_events.id;

CREATE TABLE public.platform_controls (
    hk uuid NOT NULL,
    kind text NOT NULL,
    feature text,
    starts_at timestamp with time zone NOT NULL,
    ends_at timestamp with time zone,
    announce_at timestamp with time zone NOT NULL,
    message_fa text,
    message_en text,
    reason text NOT NULL,
    allow_networks cidr[] DEFAULT '{}'::cidr[] NOT NULL,
    allow_users uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    created_by uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    lift_requested_by uuid,
    lift_requested_at timestamp with time zone,
    lifted_at timestamp with time zone,
    lifted_by uuid,
    lift_note text,
    version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT platform_controls_check CHECK (((kind = 'maintenance'::text) = (feature IS NULL))),
    CONSTRAINT platform_controls_check1 CHECK (((kind = 'maintenance'::text) OR ((cardinality(allow_networks) = 0) AND (cardinality(allow_users) = 0)))),
    CONSTRAINT platform_controls_check2 CHECK (((ends_at IS NULL) OR (ends_at > starts_at))),
    CONSTRAINT platform_controls_check3 CHECK ((announce_at <= starts_at)),
    CONSTRAINT platform_controls_check4 CHECK (((lift_requested_by IS NULL) = (lift_requested_at IS NULL))),
    CONSTRAINT platform_controls_check5 CHECK (((lifted_by IS NULL) OR (lifted_at IS NOT NULL))),
    CONSTRAINT platform_controls_feature_check CHECK ((feature = ANY (ARRAY['sign_up'::text, 'sign_in'::text, 'profile_changes'::text, 'deposits'::text, 'withdrawals'::text, 'trading'::text, 'api_keys'::text, 'websocket'::text, 'read_only'::text, 'scheduler'::text, 'outbox'::text, 'market_data'::text]))),
    CONSTRAINT platform_controls_kind_check CHECK ((kind = ANY (ARRAY['maintenance'::text, 'lockdown'::text]))),
    CONSTRAINT platform_controls_lift_note_check CHECK ((length(lift_note) <= 1000)),
    CONSTRAINT platform_controls_message_en_check CHECK ((length(message_en) <= 500)),
    CONSTRAINT platform_controls_message_fa_check CHECK ((length(message_fa) <= 500)),
    CONSTRAINT platform_controls_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 1000)))
);

CREATE TABLE public.platform_state (
    id boolean DEFAULT true NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT platform_state_id_check CHECK (id)
);

CREATE TABLE public.sanctions_screenings (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    user_id uuid,
    address_id uuid,
    counterparty_id uuid,
    screen_type public.screen_type NOT NULL,
    provider character varying(50) NOT NULL,
    provider_ref_id character varying(100),
    action public.sanction_action NOT NULL,
    risk_score numeric(5,2) DEFAULT 0 NOT NULL,
    matched_lists text[] DEFAULT '{}'::text[],
    matched_entries text[] DEFAULT '{}'::text[],
    decided_at timestamp with time zone,
    decided_by uuid,
    notes text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.sessions (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    user_id uuid NOT NULL,
    sub_account_id uuid,
    auth_method character varying(20) NOT NULL,
    access_token_hash text NOT NULL,
    refresh_token_hash text NOT NULL,
    user_agent text,
    ip inet,
    device_id character varying(64),
    status public.session_status DEFAULT 'ACTIVE'::public.session_status NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    last_used_at timestamp with time zone,
    previous_refresh_hash text,
    rotated_at timestamp with time zone,
    row_version bigint DEFAULT 0 NOT NULL,
    row_seal bytea,
    seal_key_version integer
);

CREATE TABLE public.settlements (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    type public.settlement_type NOT NULL,
    from_user_id uuid NOT NULL,
    to_user_id uuid NOT NULL,
    from_sub_account_id uuid,
    to_sub_account_id uuid,
    asset_id uuid NOT NULL,
    amount public.exact_decimal NOT NULL,
    mode public.settlement_mode DEFAULT 'OFF_CHAIN'::public.settlement_mode NOT NULL,
    from_address character varying(200),
    to_address character varying(200),
    tx_hash character varying(200),
    reference_id uuid,
    reference_type character varying(50),
    status public.settlement_status DEFAULT 'PENDING'::public.settlement_status NOT NULL,
    error_message text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    processed_at timestamp with time zone,
    completed_at timestamp with time zone
);

CREATE TABLE public.sub_accounts (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    user_id uuid NOT NULL,
    label character varying(100) NOT NULL,
    description text,
    permissions jsonb DEFAULT '[]'::jsonb NOT NULL,
    daily_volume_limit public.exact_decimal,
    daily_withdrawal_limit public.exact_decimal,
    position_limit public.exact_decimal,
    status public.sub_account_status DEFAULT 'ACTIVE'::public.sub_account_status NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);

CREATE TABLE public.trade_events (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    trade_id uuid NOT NULL,
    event_type character varying(50) NOT NULL,
    "timestamp" timestamp with time zone DEFAULT now() NOT NULL,
    market_id uuid NOT NULL,
    side public.trade_side NOT NULL,
    price public.exact_decimal NOT NULL,
    quantity public.exact_decimal NOT NULL,
    maker_order_id uuid NOT NULL,
    taker_order_id uuid NOT NULL,
    maker_user_id uuid NOT NULL,
    taker_user_id uuid NOT NULL,
    maker_fee public.exact_decimal,
    taker_fee public.exact_decimal,
    settlement_mode public.settlement_mode
);

CREATE TABLE public.trades (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    maker_order_id uuid NOT NULL,
    taker_order_id uuid NOT NULL,
    market_id uuid NOT NULL,
    side public.trade_side NOT NULL,
    price public.exact_decimal NOT NULL,
    quantity public.exact_decimal NOT NULL,
    maker_fee public.exact_decimal DEFAULT 0 NOT NULL,
    maker_fee_asset_id uuid NOT NULL,
    maker_fee_rate_bps smallint DEFAULT 0 NOT NULL,
    taker_fee public.exact_decimal DEFAULT 0 NOT NULL,
    taker_fee_asset_id uuid NOT NULL,
    taker_fee_rate_bps smallint DEFAULT 0 NOT NULL,
    settlement_mode public.settlement_mode DEFAULT 'OFF_CHAIN'::public.settlement_mode NOT NULL,
    settled boolean DEFAULT false NOT NULL,
    settled_at timestamp with time zone,
    "timestamp" timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.user_contacts (
    id bigint NOT NULL,
    user_hk uuid NOT NULL,
    type public.contact_type NOT NULL,
    value text NOT NULL,
    verified_at timestamp with time zone,
    is_primary boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    removed_at timestamp with time zone,
    ownership_checked_at timestamp with time zone,
    ownership_provider text,
    ownership_ref text,
    row_version bigint DEFAULT 0 NOT NULL,
    row_seal bytea,
    seal_key_version integer,
    CONSTRAINT user_contacts_value_check CHECK ((value <> ''::text))
);

CREATE SEQUENCE public.user_contacts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.user_contacts_id_seq OWNED BY public.user_contacts.id;

CREATE TABLE public.user_favorites (
    user_hk uuid NOT NULL,
    kind text NOT NULL,
    target_hk uuid NOT NULL,
    currency_hk uuid,
    market_id uuid,
    "position" integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_favorites_check CHECK ((((kind = 'currency'::text) AND (currency_hk = target_hk) AND (market_id IS NULL)) OR ((kind = 'pair'::text) AND (market_id = target_hk) AND (currency_hk IS NULL)))),
    CONSTRAINT user_favorites_kind_check CHECK ((kind = ANY (ARRAY['currency'::text, 'pair'::text]))),
    CONSTRAINT user_favorites_position_check CHECK (("position" >= 0))
);

CREATE TABLE public.user_national_ids (
    user_hk uuid NOT NULL,
    national_id_sealed bytea NOT NULL,
    verified_at timestamp with time zone NOT NULL,
    provider text NOT NULL,
    provider_ref text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_national_ids_provider_check CHECK ((provider <> ''::text))
);

CREATE TABLE public.user_restrictions (
    hk uuid NOT NULL,
    user_hk uuid NOT NULL,
    scope text NOT NULL,
    operations text[] DEFAULT '{}'::text[] NOT NULL,
    currency_hk uuid,
    reason text NOT NULL,
    case_ref text,
    note text,
    ends_at timestamp with time zone,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    lifted_at timestamp with time zone,
    lifted_by uuid,
    lift_note text,
    version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT user_restrictions_case_ref_check CHECK ((length(case_ref) <= 64)),
    CONSTRAINT user_restrictions_check CHECK (((scope = 'asset'::text) = (currency_hk IS NOT NULL))),
    CONSTRAINT user_restrictions_check1 CHECK (((scope = 'account'::text) = (cardinality(operations) = 0))),
    CONSTRAINT user_restrictions_check2 CHECK (((ends_at IS NULL) OR (ends_at > created_at))),
    CONSTRAINT user_restrictions_check3 CHECK (((lifted_at IS NULL) = (lifted_by IS NULL))),
    CONSTRAINT user_restrictions_lift_note_check CHECK ((length(lift_note) <= 1000)),
    CONSTRAINT user_restrictions_note_check CHECK ((length(note) <= 1000)),
    CONSTRAINT user_restrictions_operations_check CHECK ((operations <@ ARRAY['deposit'::text, 'withdraw'::text, 'convert'::text, 'buy'::text, 'sell'::text, 'trade'::text])),
    CONSTRAINT user_restrictions_reason_check CHECK ((reason ~ '^[a-z_]+$'::text)),
    CONSTRAINT user_restrictions_scope_check CHECK ((scope = ANY (ARRAY['account'::text, 'operation'::text, 'asset'::text])))
);

CREATE TABLE public.users (
    id integer NOT NULL,
    hk uuid DEFAULT gen_random_uuid() NOT NULL,
    status public.account_status_type DEFAULT 'PENDING_VERIFICATION'::public.account_status_type NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    totp_secret character varying(256),
    totp_enabled boolean DEFAULT false NOT NULL,
    first_name character varying(100),
    last_name character varying(100),
    display_name character varying(100),
    avatar_url character varying(500),
    last_login_at timestamp with time zone,
    last_login_ip inet,
    referral_code character varying(20),
    referred_by uuid,
    kyc_status public.kyc_status DEFAULT 'PENDING'::public.kyc_status NOT NULL,
    kyc_tier public.kyc_tier DEFAULT 'NONE'::public.kyc_tier NOT NULL,
    risk_score smallint DEFAULT 0 NOT NULL,
    role character varying(16) DEFAULT 'user'::character varying NOT NULL,
    totp_last_step bigint,
    security_hold_until timestamp with time zone,
    locale character varying(35),
    theme text DEFAULT 'system'::text NOT NULL,
    row_version bigint DEFAULT 0 NOT NULL,
    row_seal bytea,
    seal_key_version integer,
    CONSTRAINT users_locale_check CHECK (((locale)::text ~ '^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$'::text)),
    CONSTRAINT users_role_check CHECK (role IN ('user', 'admin')),
    CONSTRAINT users_theme_check CHECK ((theme = ANY (ARRAY['light'::text, 'dark'::text, 'system'::text])))
);

CREATE SEQUENCE public.users_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.users_id_seq OWNED BY public.users.id;

CREATE TABLE public.withdrawals (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    user_id uuid NOT NULL,
    sub_account_id uuid,
    asset_id uuid NOT NULL,
    amount public.exact_decimal NOT NULL,
    address character varying(200) NOT NULL,
    tag character varying(100),
    fee_level public.fee_level DEFAULT 'NORMAL'::public.fee_level NOT NULL,
    custom_fee public.exact_decimal,
    subtract_fee boolean DEFAULT false NOT NULL,
    priority boolean DEFAULT false NOT NULL,
    idempotency_key character varying(100) NOT NULL,
    status public.withdrawal_status_type DEFAULT 'PENDING'::public.withdrawal_status_type NOT NULL,
    tx_hash character varying(200),
    estimated_fee public.exact_decimal DEFAULT 0 NOT NULL,
    actual_fee public.exact_decimal DEFAULT 0 NOT NULL,
    net_amount public.exact_decimal DEFAULT 0 NOT NULL,
    confirmations integer DEFAULT 0 NOT NULL,
    required_confirms integer DEFAULT 1 NOT NULL,
    broadcast_at timestamp with time zone,
    confirmed_at timestamp with time zone,
    error_message text,
    raw_data jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.market_candles ATTACH PARTITION public.market_candles_15m FOR VALUES IN ('15m');

ALTER TABLE ONLY public.market_candles ATTACH PARTITION public.market_candles_1d FOR VALUES IN ('1d');

ALTER TABLE ONLY public.market_candles ATTACH PARTITION public.market_candles_1h FOR VALUES IN ('1h');

ALTER TABLE ONLY public.market_candles ATTACH PARTITION public.market_candles_1m FOR VALUES IN ('1m');

ALTER TABLE ONLY public.market_candles ATTACH PARTITION public.market_candles_5m FOR VALUES IN ('5m');

ALTER TABLE ONLY public.audit_events ALTER COLUMN id SET DEFAULT nextval('public.audit_events_id_seq'::regclass);

ALTER TABLE ONLY public.currencies ALTER COLUMN id SET DEFAULT nextval('public.currencies_id_seq'::regclass);

ALTER TABLE ONLY public.integrity_findings ALTER COLUMN id SET DEFAULT nextval('public.integrity_findings_id_seq'::regclass);

ALTER TABLE ONLY public.outbox_events ALTER COLUMN id SET DEFAULT nextval('public.outbox_events_id_seq'::regclass);

ALTER TABLE ONLY public.user_contacts ALTER COLUMN id SET DEFAULT nextval('public.user_contacts_id_seq'::regclass);

ALTER TABLE ONLY public.users ALTER COLUMN id SET DEFAULT nextval('public.users_id_seq'::regclass);

ALTER TABLE ONLY public.aml_alerts
    ADD CONSTRAINT aml_alerts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.aml_rules
    ADD CONSTRAINT aml_rules_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_public_key_key UNIQUE (public_key);

ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_symbol_key UNIQUE (symbol);

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_hk_key UNIQUE (hk);

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.balance_changes
    ADD CONSTRAINT balance_changes_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.balance_snapshots
    ADD CONSTRAINT balance_snapshots_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.balances
    ADD CONSTRAINT balances_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.currencies
    ADD CONSTRAINT currencies_hk_key UNIQUE (hk);

ALTER TABLE ONLY public.currencies
    ADD CONSTRAINT currencies_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.currency_capability_overrides
    ADD CONSTRAINT currency_capability_overrides_pkey PRIMARY KEY (currency_hk, capability);

ALTER TABLE ONLY public.currency_delisting_steps
    ADD CONSTRAINT currency_delisting_steps_pkey PRIMARY KEY (delisting_hk, state);

ALTER TABLE ONLY public.currency_delistings
    ADD CONSTRAINT currency_delistings_pkey PRIMARY KEY (hk);

ALTER TABLE ONLY public.currency_migration_phases
    ADD CONSTRAINT currency_migration_phases_pkey PRIMARY KEY (migration_hk, phase);

ALTER TABLE ONLY public.currency_migration_sources
    ADD CONSTRAINT currency_migration_sources_pkey PRIMARY KEY (migration_hk, currency_hk);

ALTER TABLE ONLY public.currency_migrations
    ADD CONSTRAINT currency_migrations_pkey PRIMARY KEY (hk);

ALTER TABLE ONLY public.currency_symbols
    ADD CONSTRAINT currency_symbols_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.custody_configs
    ADD CONSTRAINT custody_configs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.data_keys
    ADD CONSTRAINT data_keys_pkey PRIMARY KEY (purpose, version);

ALTER TABLE ONLY public.deposit_addresses
    ADD CONSTRAINT deposit_addresses_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.deposits
    ADD CONSTRAINT deposits_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.integrity_findings
    ADD CONSTRAINT integrity_findings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.kyc_applications
    ADD CONSTRAINT kyc_applications_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.kyc_documents
    ADD CONSTRAINT kyc_documents_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.kyc_profiles
    ADD CONSTRAINT kyc_profiles_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.kyc_profiles
    ADD CONSTRAINT kyc_profiles_user_id_key UNIQUE (user_id);

ALTER TABLE ONLY public.market_asset_contracts
    ADD CONSTRAINT market_asset_contracts_pkey PRIMARY KEY (network, address);

ALTER TABLE ONLY public.market_asset_sources
    ADD CONSTRAINT market_asset_sources_pkey PRIMARY KEY (source, external_id);

ALTER TABLE ONLY public.market_assets
    ADD CONSTRAINT market_assets_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.market_assets
    ADD CONSTRAINT market_assets_slug_key UNIQUE (slug);

ALTER TABLE ONLY public.market_candle_cursors
    ADD CONSTRAINT market_candle_cursors_pkey PRIMARY KEY (pair_id, resolution);

ALTER TABLE ONLY public.market_candles
    ADD CONSTRAINT market_candles_pkey PRIMARY KEY (pair_id, resolution, open_time);

ALTER TABLE ONLY public.market_candles_15m
    ADD CONSTRAINT market_candles_15m_pkey PRIMARY KEY (pair_id, resolution, open_time);

ALTER TABLE ONLY public.market_candles_1d
    ADD CONSTRAINT market_candles_1d_pkey PRIMARY KEY (pair_id, resolution, open_time);

ALTER TABLE ONLY public.market_candles_1h
    ADD CONSTRAINT market_candles_1h_pkey PRIMARY KEY (pair_id, resolution, open_time);

ALTER TABLE ONLY public.market_candles_1m
    ADD CONSTRAINT market_candles_1m_pkey PRIMARY KEY (pair_id, resolution, open_time);

ALTER TABLE ONLY public.market_candles_5m
    ADD CONSTRAINT market_candles_5m_pkey PRIMARY KEY (pair_id, resolution, open_time);

ALTER TABLE ONLY public.market_identity_conflicts
    ADD CONSTRAINT market_identity_conflicts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.market_job_leases
    ADD CONSTRAINT market_job_leases_pkey PRIMARY KEY (job);

ALTER TABLE ONLY public.market_source_controls
    ADD CONSTRAINT market_source_controls_pkey PRIMARY KEY (source);

ALTER TABLE ONLY public.market_source_usage
    ADD CONSTRAINT market_source_usage_pkey PRIMARY KEY (source, window_kind);

ALTER TABLE ONLY public.market_stats_24h
    ADD CONSTRAINT market_stats_24h_pkey PRIMARY KEY (market_id, "timestamp");

ALTER TABLE ONLY public.market_sync_runs
    ADD CONSTRAINT market_sync_runs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.market_tickers_latest
    ADD CONSTRAINT market_tickers_latest_pkey PRIMARY KEY (pair_id);

ALTER TABLE ONLY public.market_venue_pairs
    ADD CONSTRAINT market_venue_pairs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.market_venue_pairs
    ADD CONSTRAINT market_venue_pairs_venue_id_venue_symbol_key UNIQUE (venue_id, venue_symbol);

ALTER TABLE ONLY public.market_venues
    ADD CONSTRAINT market_venues_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.markets
    ADD CONSTRAINT markets_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.order_events
    ADD CONSTRAINT order_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.outbox_events
    ADD CONSTRAINT outbox_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.platform_controls
    ADD CONSTRAINT platform_controls_pkey PRIMARY KEY (hk);

ALTER TABLE ONLY public.platform_state
    ADD CONSTRAINT platform_state_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.sanctions_screenings
    ADD CONSTRAINT sanctions_screenings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.settlements
    ADD CONSTRAINT settlements_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.sub_accounts
    ADD CONSTRAINT sub_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.trade_events
    ADD CONSTRAINT trade_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.trades
    ADD CONSTRAINT trades_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT uq_client_order_id UNIQUE (user_id, client_order_id);

ALTER TABLE ONLY public.custody_configs
    ADD CONSTRAINT uq_custody_asset_network UNIQUE (asset_id, network);

ALTER TABLE ONLY public.deposits
    ADD CONSTRAINT uq_deposit_tx_asset UNIQUE (tx_hash, asset_id);

ALTER TABLE ONLY public.markets
    ADD CONSTRAINT uq_market_pair UNIQUE (base_asset_id, quote_asset_id);

ALTER TABLE ONLY public.deposit_addresses
    ADD CONSTRAINT uq_user_asset_address UNIQUE (user_id, sub_account_id, asset_id, address);

ALTER TABLE ONLY public.balances
    ADD CONSTRAINT uq_user_asset_sub UNIQUE NULLS NOT DISTINCT (user_id, sub_account_id, asset_id);

ALTER TABLE ONLY public.user_contacts
    ADD CONSTRAINT user_contacts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.user_favorites
    ADD CONSTRAINT user_favorites_pkey PRIMARY KEY (user_hk, kind, target_hk);

ALTER TABLE ONLY public.user_national_ids
    ADD CONSTRAINT user_national_ids_pkey PRIMARY KEY (user_hk);

ALTER TABLE ONLY public.user_restrictions
    ADD CONSTRAINT user_restrictions_pkey PRIMARY KEY (hk);

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_hk_key UNIQUE (hk);

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_referral_code_key UNIQUE (referral_code);

ALTER TABLE ONLY public.withdrawals
    ADD CONSTRAINT withdrawals_idempotency_key_key UNIQUE (idempotency_key);

ALTER TABLE ONLY public.withdrawals
    ADD CONSTRAINT withdrawals_pkey PRIMARY KEY (id);

CREATE INDEX audit_events_action ON public.audit_events USING btree (action, hk);

CREATE INDEX audit_events_actor ON public.audit_events USING btree (actor_id, hk) WHERE (actor_id IS NOT NULL);

CREATE INDEX audit_events_target ON public.audit_events USING btree (target_type, target_id, hk) WHERE (target_id IS NOT NULL);

CREATE UNIQUE INDEX currencies_info_contract_unique ON public.currencies USING btree (((info ->> 'contract'::text))) WHERE ((info ->> 'contract'::text) <> ''::text);

CREATE UNIQUE INDEX currencies_info_symbol_unique ON public.currencies USING btree (((info ->> 'symbol'::text)));

CREATE UNIQUE INDEX currency_symbols_reserved ON public.currency_symbols USING btree (symbol) WHERE (released_at IS NULL);

CREATE INDEX idx_aml_alerts_created ON public.aml_alerts USING btree (created_at);

CREATE INDEX idx_aml_alerts_rule_id ON public.aml_alerts USING btree (rule_id);

CREATE INDEX idx_aml_alerts_status ON public.aml_alerts USING btree (status);

CREATE INDEX idx_aml_alerts_user_id ON public.aml_alerts USING btree (user_id);

CREATE INDEX idx_aml_rules_enabled ON public.aml_rules USING btree (enabled);

CREATE INDEX idx_aml_rules_type ON public.aml_rules USING btree (rule_type);

CREATE INDEX idx_api_keys_public_key ON public.api_keys USING btree (public_key);

CREATE INDEX idx_api_keys_status ON public.api_keys USING btree (status);

CREATE INDEX idx_api_keys_sub_account_id ON public.api_keys USING btree (sub_account_id);

CREATE INDEX idx_assets_contract ON public.assets USING btree (contract_address);

CREATE INDEX idx_assets_deleted ON public.assets USING btree (deleted_at) WHERE (deleted_at IS NOT NULL);

CREATE INDEX idx_assets_network ON public.assets USING btree (network);

CREATE INDEX idx_assets_symbol ON public.assets USING btree (symbol);

CREATE INDEX idx_assets_type_status ON public.assets USING btree (type, status);

CREATE INDEX idx_balance_changes_balance_id ON public.balance_changes USING btree (balance_id);

CREATE INDEX idx_balance_changes_reference ON public.balance_changes USING btree (reference_id, reference_type);

CREATE INDEX idx_balance_changes_timestamp ON public.balance_changes USING btree ("timestamp");

CREATE INDEX idx_balance_changes_user_id ON public.balance_changes USING btree (user_id);

CREATE INDEX idx_balance_snapshots_balance_id ON public.balance_snapshots USING btree (balance_id);

CREATE INDEX idx_balance_snapshots_timestamp ON public.balance_snapshots USING btree ("timestamp");

CREATE INDEX idx_balances_asset_id ON public.balances USING btree (asset_id);

CREATE INDEX idx_balances_sub_account_id ON public.balances USING btree (sub_account_id);

CREATE INDEX idx_balances_total ON public.balances USING btree (total DESC);

CREATE INDEX idx_balances_user_id ON public.balances USING btree (user_id);

CREATE INDEX idx_currencies_created_hk ON public.currencies USING btree (created_at DESC, hk DESC);

CREATE INDEX idx_currency_delisting_steps_due ON public.currency_delisting_steps USING btree (scheduled_at) WHERE (applied_at IS NULL);

CREATE INDEX idx_currency_delistings_currency ON public.currency_delistings USING btree (currency_hk, created_at DESC);

CREATE UNIQUE INDEX idx_currency_delistings_one_open ON public.currency_delistings USING btree (currency_hk) WHERE (status = 'scheduled'::text);

CREATE INDEX idx_currency_migration_phases_due ON public.currency_migration_phases USING btree (scheduled_at) WHERE (applied_at IS NULL);

CREATE INDEX idx_currency_migration_sources_currency ON public.currency_migration_sources USING btree (currency_hk);

CREATE INDEX idx_currency_migrations_target ON public.currency_migrations USING btree (target_hk, created_at DESC);

CREATE INDEX idx_currency_symbols_currency ON public.currency_symbols USING btree (currency_hk, created_at DESC);

CREATE UNIQUE INDEX idx_currency_symbols_current ON public.currency_symbols USING btree (currency_hk) WHERE current;

CREATE INDEX idx_custody_configs_asset ON public.custody_configs USING btree (asset_id);

CREATE INDEX idx_custody_configs_provider ON public.custody_configs USING btree (provider_type);

CREATE INDEX idx_deposit_addresses_address ON public.deposit_addresses USING btree (address);

CREATE INDEX idx_deposit_addresses_asset_id ON public.deposit_addresses USING btree (asset_id);

CREATE INDEX idx_deposit_addresses_user_id ON public.deposit_addresses USING btree (user_id);

CREATE INDEX idx_deposits_address ON public.deposits USING btree (address);

CREATE INDEX idx_deposits_asset_id ON public.deposits USING btree (asset_id);

CREATE INDEX idx_deposits_detected_at ON public.deposits USING btree (detected_at);

CREATE INDEX idx_deposits_status ON public.deposits USING btree (status);

CREATE INDEX idx_deposits_tx_hash ON public.deposits USING btree (tx_hash);

CREATE INDEX idx_kyc_applications_status ON public.kyc_applications USING btree (status);

CREATE INDEX idx_kyc_applications_user_id ON public.kyc_applications USING btree (user_id);

CREATE INDEX idx_kyc_documents_profile_id ON public.kyc_documents USING btree (kyc_profile_id);

CREATE INDEX idx_kyc_documents_status ON public.kyc_documents USING btree (status);

CREATE INDEX idx_kyc_profiles_status ON public.kyc_profiles USING btree (status);

CREATE INDEX idx_kyc_profiles_tier ON public.kyc_profiles USING btree (tier);

CREATE INDEX idx_kyc_profiles_user_id ON public.kyc_profiles USING btree (user_id);

CREATE INDEX idx_markets_base_asset ON public.markets USING btree (base_asset_id);

CREATE INDEX idx_markets_deleted ON public.markets USING btree (deleted_at) WHERE (deleted_at IS NOT NULL);

CREATE INDEX idx_markets_quote_asset ON public.markets USING btree (quote_asset_id);

CREATE INDEX idx_markets_status ON public.markets USING btree (status);

CREATE INDEX idx_order_events_order_id ON public.order_events USING btree (order_id);

CREATE INDEX idx_order_events_timestamp ON public.order_events USING btree ("timestamp");

CREATE INDEX idx_orders_client_order_id ON public.orders USING btree (client_order_id);

CREATE INDEX idx_orders_created_at ON public.orders USING btree (created_at DESC);

CREATE INDEX idx_orders_market_id ON public.orders USING btree (market_id);

CREATE INDEX idx_orders_market_status ON public.orders USING btree (market_id, status);

CREATE INDEX idx_orders_status ON public.orders USING btree (status);

CREATE INDEX idx_orders_sub_account_id ON public.orders USING btree (sub_account_id);

CREATE INDEX idx_orders_user_id ON public.orders USING btree (user_id);

CREATE INDEX idx_platform_controls_created ON public.platform_controls USING btree (created_at DESC, hk DESC);

CREATE INDEX idx_platform_controls_open ON public.platform_controls USING btree (starts_at) WHERE (lifted_at IS NULL);

CREATE INDEX idx_sanctions_action ON public.sanctions_screenings USING btree (action);

CREATE INDEX idx_sanctions_created ON public.sanctions_screenings USING btree (created_at);

CREATE INDEX idx_sanctions_screen_type ON public.sanctions_screenings USING btree (screen_type);

CREATE INDEX idx_sanctions_user_id ON public.sanctions_screenings USING btree (user_id);

CREATE INDEX idx_sessions_expires_at ON public.sessions USING btree (expires_at);

CREATE INDEX idx_sessions_refresh_token ON public.sessions USING btree (refresh_token_hash);

CREATE INDEX idx_sessions_status ON public.sessions USING btree (status);

CREATE INDEX idx_sessions_sub_account_id ON public.sessions USING btree (sub_account_id);

CREATE INDEX idx_sessions_user_active ON public.sessions USING btree (user_id, created_at DESC, id DESC) WHERE (status = 'ACTIVE'::public.session_status);

CREATE INDEX idx_sessions_user_id ON public.sessions USING btree (user_id);

CREATE INDEX idx_settlements_asset ON public.settlements USING btree (asset_id);

CREATE INDEX idx_settlements_created ON public.settlements USING btree (created_at);

CREATE INDEX idx_settlements_from_user ON public.settlements USING btree (from_user_id);

CREATE INDEX idx_settlements_reference ON public.settlements USING btree (reference_id, reference_type);

CREATE INDEX idx_settlements_status ON public.settlements USING btree (status);

CREATE INDEX idx_settlements_to_user ON public.settlements USING btree (to_user_id);

CREATE INDEX idx_settlements_type ON public.settlements USING btree (type);

CREATE INDEX idx_sub_accounts_deleted ON public.sub_accounts USING btree (deleted_at) WHERE (deleted_at IS NOT NULL);

CREATE INDEX idx_sub_accounts_status ON public.sub_accounts USING btree (status);

CREATE INDEX idx_sub_accounts_user_id ON public.sub_accounts USING btree (user_id);

CREATE INDEX idx_trade_events_timestamp ON public.trade_events USING btree ("timestamp");

CREATE INDEX idx_trade_events_trade_id ON public.trade_events USING btree (trade_id);

CREATE INDEX idx_trades_maker_order ON public.trades USING btree (maker_order_id);

CREATE INDEX idx_trades_market_id ON public.trades USING btree (market_id);

CREATE INDEX idx_trades_market_time ON public.trades USING btree (market_id, "timestamp" DESC);

CREATE INDEX idx_trades_taker_order ON public.trades USING btree (taker_order_id);

CREATE INDEX idx_trades_timestamp ON public.trades USING btree ("timestamp" DESC);

CREATE INDEX idx_user_restrictions_open ON public.user_restrictions USING btree (user_hk) WHERE (lifted_at IS NULL);

CREATE INDEX idx_user_restrictions_user_created ON public.user_restrictions USING btree (user_hk, created_at DESC, hk DESC);

CREATE INDEX idx_users_kyc_status ON public.users USING btree (kyc_status);

CREATE INDEX idx_users_referral_code ON public.users USING btree (referral_code);

CREATE INDEX idx_users_status ON public.users USING btree (status);

CREATE INDEX idx_withdrawals_asset_id ON public.withdrawals USING btree (asset_id);

CREATE INDEX idx_withdrawals_created ON public.withdrawals USING btree (created_at);

CREATE INDEX idx_withdrawals_idempotency ON public.withdrawals USING btree (idempotency_key);

CREATE INDEX idx_withdrawals_status ON public.withdrawals USING btree (status);

CREATE INDEX idx_withdrawals_user_id ON public.withdrawals USING btree (user_id);

CREATE INDEX integrity_findings_newest ON public.integrity_findings USING btree (id DESC);

CREATE UNIQUE INDEX integrity_findings_open ON public.integrity_findings USING btree (table_name, row_key) WHERE (resolved_at IS NULL);

CREATE INDEX market_asset_contracts_asset ON public.market_asset_contracts USING btree (asset_id);

CREATE INDEX market_asset_sources_asset ON public.market_asset_sources USING btree (asset_id);

CREATE INDEX market_asset_sources_details ON public.market_asset_sources USING btree (source, details_fetched_at NULLS FIRST);

CREATE UNIQUE INDEX market_assets_currency ON public.market_assets USING btree (currency_hk) WHERE (currency_hk IS NOT NULL);

CREATE INDEX market_assets_newest ON public.market_assets USING btree (created_at DESC, slug) WHERE (status <> 'hidden'::text);

CREATE INDEX market_assets_rank ON public.market_assets USING btree (market_cap_rank) WHERE (market_cap_rank IS NOT NULL);

CREATE INDEX market_assets_symbol ON public.market_assets USING btree (symbol);

CREATE INDEX market_assets_updated ON public.market_assets USING btree (updated_at, slug);

CREATE UNIQUE INDEX market_identity_conflicts_open ON public.market_identity_conflicts USING btree (source, external_id, kind, subject) WHERE (resolved_at IS NULL);

CREATE INDEX market_identity_conflicts_unresolved ON public.market_identity_conflicts USING btree (id) WHERE (resolved_at IS NULL);

CREATE INDEX market_sync_runs_job ON public.market_sync_runs USING btree (job, started_at DESC);

CREATE INDEX market_sync_runs_started ON public.market_sync_runs USING btree (started_at DESC, id DESC);

CREATE INDEX market_venue_pairs_base ON public.market_venue_pairs USING btree (base_asset_id) WHERE (base_asset_id IS NOT NULL);

CREATE INDEX outbox_events_failed ON public.outbox_events USING btree (id) WHERE (failed_at IS NOT NULL);

CREATE INDEX outbox_events_pending ON public.outbox_events USING btree (topic, id) WHERE ((published_at IS NULL) AND (failed_at IS NULL));

CREATE INDEX outbox_events_published ON public.outbox_events USING btree (published_at) WHERE (published_at IS NOT NULL);

CREATE UNIQUE INDEX user_contacts_primary_unique ON public.user_contacts USING btree (user_hk, type) WHERE (is_primary AND (removed_at IS NULL));

CREATE INDEX user_contacts_user_hk ON public.user_contacts USING btree (user_hk);

CREATE UNIQUE INDEX user_contacts_value_unique ON public.user_contacts USING btree (type, lower((value COLLATE "C"))) WHERE (removed_at IS NULL);

ALTER INDEX public.market_candles_pkey ATTACH PARTITION public.market_candles_15m_pkey;

ALTER INDEX public.market_candles_pkey ATTACH PARTITION public.market_candles_1d_pkey;

ALTER INDEX public.market_candles_pkey ATTACH PARTITION public.market_candles_1h_pkey;

ALTER INDEX public.market_candles_pkey ATTACH PARTITION public.market_candles_1m_pkey;

ALTER INDEX public.market_candles_pkey ATTACH PARTITION public.market_candles_5m_pkey;

CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON public.audit_events FOR EACH STATEMENT EXECUTE FUNCTION public.audit_events_append_only();

CREATE TRIGGER audit_events_no_update_or_delete BEFORE DELETE OR UPDATE ON public.audit_events FOR EACH ROW EXECUTE FUNCTION public.audit_events_append_only();

CREATE TRIGGER currency_symbols_follow AFTER INSERT OR UPDATE OF info ON public.currencies FOR EACH ROW EXECUTE FUNCTION public.currency_symbols_follow();

CREATE TRIGGER update_aml_alerts_updated_at BEFORE UPDATE ON public.aml_alerts FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_aml_rules_updated_at BEFORE UPDATE ON public.aml_rules FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_api_keys_updated_at BEFORE UPDATE ON public.api_keys FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_assets_updated_at BEFORE UPDATE ON public.assets FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_balances_updated_at BEFORE UPDATE ON public.balances FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_custody_configs_updated_at BEFORE UPDATE ON public.custody_configs FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_kyc_applications_updated_at BEFORE UPDATE ON public.kyc_applications FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_kyc_documents_updated_at BEFORE UPDATE ON public.kyc_documents FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_kyc_profiles_updated_at BEFORE UPDATE ON public.kyc_profiles FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_markets_updated_at BEFORE UPDATE ON public.markets FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_orders_updated_at BEFORE UPDATE ON public.orders FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_sub_accounts_updated_at BEFORE UPDATE ON public.sub_accounts FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

CREATE TRIGGER update_withdrawals_updated_at BEFORE UPDATE ON public.withdrawals FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

ALTER TABLE ONLY public.aml_alerts
    ADD CONSTRAINT aml_alerts_assigned_to_fkey FOREIGN KEY (assigned_to) REFERENCES public.users(hk);

ALTER TABLE ONLY public.aml_alerts
    ADD CONSTRAINT aml_alerts_resolved_by_fkey FOREIGN KEY (resolved_by) REFERENCES public.users(hk);

ALTER TABLE ONLY public.aml_alerts
    ADD CONSTRAINT aml_alerts_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.aml_rules(id);

ALTER TABLE ONLY public.aml_alerts
    ADD CONSTRAINT aml_alerts_sub_account_id_fkey FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE ONLY public.aml_alerts
    ADD CONSTRAINT aml_alerts_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk);

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_sub_account_id_fkey FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.balance_changes
    ADD CONSTRAINT balance_changes_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.balance_changes
    ADD CONSTRAINT balance_changes_balance_id_fkey FOREIGN KEY (balance_id) REFERENCES public.balances(id);

ALTER TABLE ONLY public.balance_changes
    ADD CONSTRAINT balance_changes_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk);

ALTER TABLE ONLY public.balance_snapshots
    ADD CONSTRAINT balance_snapshots_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.balance_snapshots
    ADD CONSTRAINT balance_snapshots_balance_id_fkey FOREIGN KEY (balance_id) REFERENCES public.balances(id);

ALTER TABLE ONLY public.balance_snapshots
    ADD CONSTRAINT balance_snapshots_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk);

ALTER TABLE ONLY public.balances
    ADD CONSTRAINT balances_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.balances
    ADD CONSTRAINT balances_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk);

ALTER TABLE ONLY public.currency_capability_overrides
    ADD CONSTRAINT currency_capability_overrides_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES public.currencies(hk);

ALTER TABLE ONLY public.currency_delisting_steps
    ADD CONSTRAINT currency_delisting_steps_delisting_hk_fkey FOREIGN KEY (delisting_hk) REFERENCES public.currency_delistings(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.currency_delistings
    ADD CONSTRAINT currency_delistings_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES public.currencies(hk);

ALTER TABLE ONLY public.currency_delistings
    ADD CONSTRAINT currency_delistings_settlement_currency_hk_fkey FOREIGN KEY (settlement_currency_hk) REFERENCES public.currencies(hk);

ALTER TABLE ONLY public.currency_migration_phases
    ADD CONSTRAINT currency_migration_phases_migration_hk_fkey FOREIGN KEY (migration_hk) REFERENCES public.currency_migrations(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.currency_migration_sources
    ADD CONSTRAINT currency_migration_sources_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES public.currencies(hk);

ALTER TABLE ONLY public.currency_migration_sources
    ADD CONSTRAINT currency_migration_sources_migration_hk_fkey FOREIGN KEY (migration_hk) REFERENCES public.currency_migrations(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.currency_migrations
    ADD CONSTRAINT currency_migrations_target_hk_fkey FOREIGN KEY (target_hk) REFERENCES public.currencies(hk);

ALTER TABLE ONLY public.currency_symbols
    ADD CONSTRAINT currency_symbols_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES public.currencies(hk);

ALTER TABLE ONLY public.custody_configs
    ADD CONSTRAINT custody_configs_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.custody_configs
    ADD CONSTRAINT custody_configs_fee_asset_id_fkey FOREIGN KEY (fee_asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.deposit_addresses
    ADD CONSTRAINT deposit_addresses_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.deposit_addresses
    ADD CONSTRAINT deposit_addresses_sub_account_id_fkey FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.deposit_addresses
    ADD CONSTRAINT deposit_addresses_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.deposits
    ADD CONSTRAINT deposits_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.balance_changes
    ADD CONSTRAINT fk_balance_changes_sub_account FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE ONLY public.balance_snapshots
    ADD CONSTRAINT fk_balance_snapshots_sub_account FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE ONLY public.balances
    ADD CONSTRAINT fk_balances_sub_account FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE ONLY public.order_events
    ADD CONSTRAINT fk_order_events_trade_id FOREIGN KEY (trade_id) REFERENCES public.trades(id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT fk_orders_sub_account FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE ONLY public.kyc_applications
    ADD CONSTRAINT kyc_applications_reviewed_by_fkey FOREIGN KEY (reviewed_by) REFERENCES public.users(hk);

ALTER TABLE ONLY public.kyc_applications
    ADD CONSTRAINT kyc_applications_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.kyc_documents
    ADD CONSTRAINT kyc_documents_kyc_profile_id_fkey FOREIGN KEY (kyc_profile_id) REFERENCES public.kyc_profiles(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kyc_documents
    ADD CONSTRAINT kyc_documents_verified_by_fkey FOREIGN KEY (verified_by) REFERENCES public.users(hk);

ALTER TABLE ONLY public.kyc_profiles
    ADD CONSTRAINT kyc_profiles_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.kyc_profiles
    ADD CONSTRAINT kyc_profiles_verified_by_fkey FOREIGN KEY (verified_by) REFERENCES public.users(hk);

ALTER TABLE ONLY public.market_asset_contracts
    ADD CONSTRAINT market_asset_contracts_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.market_assets(id);

ALTER TABLE ONLY public.market_asset_sources
    ADD CONSTRAINT market_asset_sources_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.market_assets(id);

ALTER TABLE ONLY public.market_candle_cursors
    ADD CONSTRAINT market_candle_cursors_pair_id_fkey FOREIGN KEY (pair_id) REFERENCES public.market_venue_pairs(id);

ALTER TABLE public.market_candles
    ADD CONSTRAINT market_candles_pair_id_fkey FOREIGN KEY (pair_id) REFERENCES public.market_venue_pairs(id);

ALTER TABLE ONLY public.market_identity_conflicts
    ADD CONSTRAINT market_identity_conflicts_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.market_assets(id);

ALTER TABLE ONLY public.market_identity_conflicts
    ADD CONSTRAINT market_identity_conflicts_claimed_by_fkey FOREIGN KEY (claimed_by) REFERENCES public.market_assets(id);

ALTER TABLE ONLY public.market_stats_24h
    ADD CONSTRAINT market_stats_24h_market_id_fkey FOREIGN KEY (market_id) REFERENCES public.markets(id);

ALTER TABLE ONLY public.market_tickers_latest
    ADD CONSTRAINT market_tickers_latest_pair_id_fkey FOREIGN KEY (pair_id) REFERENCES public.market_venue_pairs(id);

ALTER TABLE ONLY public.market_venue_pairs
    ADD CONSTRAINT market_venue_pairs_base_asset_id_fkey FOREIGN KEY (base_asset_id) REFERENCES public.market_assets(id);

ALTER TABLE ONLY public.market_venue_pairs
    ADD CONSTRAINT market_venue_pairs_quote_asset_id_fkey FOREIGN KEY (quote_asset_id) REFERENCES public.market_assets(id);

ALTER TABLE ONLY public.market_venue_pairs
    ADD CONSTRAINT market_venue_pairs_venue_id_fkey FOREIGN KEY (venue_id) REFERENCES public.market_venues(id);

ALTER TABLE ONLY public.markets
    ADD CONSTRAINT markets_base_asset_id_fkey FOREIGN KEY (base_asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.markets
    ADD CONSTRAINT markets_quote_asset_id_fkey FOREIGN KEY (quote_asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.order_events
    ADD CONSTRAINT order_events_order_id_fkey FOREIGN KEY (order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_fee_asset_id_fkey FOREIGN KEY (fee_asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_market_id_fkey FOREIGN KEY (market_id) REFERENCES public.markets(id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk);

ALTER TABLE ONLY public.sanctions_screenings
    ADD CONSTRAINT sanctions_screenings_decided_by_fkey FOREIGN KEY (decided_by) REFERENCES public.users(hk);

ALTER TABLE ONLY public.sanctions_screenings
    ADD CONSTRAINT sanctions_screenings_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk) ON DELETE SET NULL;

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_sub_account_id_fkey FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.settlements
    ADD CONSTRAINT settlements_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.settlements
    ADD CONSTRAINT settlements_from_sub_account_id_fkey FOREIGN KEY (from_sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE ONLY public.settlements
    ADD CONSTRAINT settlements_from_user_id_fkey FOREIGN KEY (from_user_id) REFERENCES public.users(hk);

ALTER TABLE ONLY public.settlements
    ADD CONSTRAINT settlements_to_sub_account_id_fkey FOREIGN KEY (to_sub_account_id) REFERENCES public.sub_accounts(id);

ALTER TABLE ONLY public.settlements
    ADD CONSTRAINT settlements_to_user_id_fkey FOREIGN KEY (to_user_id) REFERENCES public.users(hk);

ALTER TABLE ONLY public.sub_accounts
    ADD CONSTRAINT sub_accounts_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.trade_events
    ADD CONSTRAINT trade_events_maker_order_id_fkey FOREIGN KEY (maker_order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.trade_events
    ADD CONSTRAINT trade_events_maker_user_id_fkey FOREIGN KEY (maker_user_id) REFERENCES public.users(hk);

ALTER TABLE ONLY public.trade_events
    ADD CONSTRAINT trade_events_market_id_fkey FOREIGN KEY (market_id) REFERENCES public.markets(id);

ALTER TABLE ONLY public.trade_events
    ADD CONSTRAINT trade_events_taker_order_id_fkey FOREIGN KEY (taker_order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.trade_events
    ADD CONSTRAINT trade_events_taker_user_id_fkey FOREIGN KEY (taker_user_id) REFERENCES public.users(hk);

ALTER TABLE ONLY public.trade_events
    ADD CONSTRAINT trade_events_trade_id_fkey FOREIGN KEY (trade_id) REFERENCES public.trades(id);

ALTER TABLE ONLY public.trades
    ADD CONSTRAINT trades_maker_fee_asset_id_fkey FOREIGN KEY (maker_fee_asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.trades
    ADD CONSTRAINT trades_maker_order_id_fkey FOREIGN KEY (maker_order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.trades
    ADD CONSTRAINT trades_market_id_fkey FOREIGN KEY (market_id) REFERENCES public.markets(id);

ALTER TABLE ONLY public.trades
    ADD CONSTRAINT trades_taker_fee_asset_id_fkey FOREIGN KEY (taker_fee_asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.trades
    ADD CONSTRAINT trades_taker_order_id_fkey FOREIGN KEY (taker_order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.user_contacts
    ADD CONSTRAINT user_contacts_user_hk_fkey FOREIGN KEY (user_hk) REFERENCES public.users(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.user_favorites
    ADD CONSTRAINT user_favorites_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES public.currencies(hk);

ALTER TABLE ONLY public.user_favorites
    ADD CONSTRAINT user_favorites_market_id_fkey FOREIGN KEY (market_id) REFERENCES public.markets(id);

ALTER TABLE ONLY public.user_favorites
    ADD CONSTRAINT user_favorites_user_hk_fkey FOREIGN KEY (user_hk) REFERENCES public.users(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.user_national_ids
    ADD CONSTRAINT user_national_ids_user_hk_fkey FOREIGN KEY (user_hk) REFERENCES public.users(hk) ON DELETE CASCADE;

ALTER TABLE ONLY public.user_restrictions
    ADD CONSTRAINT user_restrictions_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES public.currencies(hk);

ALTER TABLE ONLY public.user_restrictions
    ADD CONSTRAINT user_restrictions_user_hk_fkey FOREIGN KEY (user_hk) REFERENCES public.users(hk);

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_referred_by_fkey FOREIGN KEY (referred_by) REFERENCES public.users(hk);

ALTER TABLE ONLY public.withdrawals
    ADD CONSTRAINT withdrawals_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE ONLY public.withdrawals
    ADD CONSTRAINT withdrawals_sub_account_id_fkey FOREIGN KEY (sub_account_id) REFERENCES public.sub_accounts(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.withdrawals
    ADD CONSTRAINT withdrawals_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(hk) ON DELETE CASCADE;

-- The state of the platform controls is one row, which every instance reads.
INSERT INTO public.platform_state DEFAULT VALUES;
