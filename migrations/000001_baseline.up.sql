-- 000001_baseline — the full tugbot schema + seed data (generated once, ADR 0002
-- pg_dump --schema-only against the diesel-provisioned compose PG, with the
-- five feature-flag seed INSERTs grafted in — 2026-09-03). Never re-dump
-- the production DB (drift risk).

--
--
--
--
CREATE TYPE public.job_status AS ENUM (
    'created',
    'running',
    'done',
    'failure'
);
--
--
CREATE FUNCTION public.diesel_manage_updated_at(_tbl regclass) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    EXECUTE format('CREATE TRIGGER set_updated_at BEFORE UPDATE ON %s
                    FOR EACH ROW EXECUTE PROCEDURE diesel_set_updated_at()', _tbl);
END;
$$;
--
--
CREATE FUNCTION public.diesel_set_updated_at() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (
        NEW IS DISTINCT FROM OLD AND
        NEW.updated_at IS NOT DISTINCT FROM OLD.updated_at
    ) THEN
        NEW.updated_at := current_timestamp;
    END IF;
    RETURN NEW;
END;
$$;
--
--
CREATE TABLE public.__diesel_schema_migrations (
    version character varying(50) NOT NULL,
    run_on timestamp without time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);
--
--
CREATE TABLE public.ai_slop_usage (
    id integer NOT NULL,
    user_id bigint NOT NULL,
    guild_id bigint NOT NULL,
    usage_count integer DEFAULT 0 NOT NULL,
    last_slop_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);
--
--
CREATE SEQUENCE public.ai_slop_usage_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.ai_slop_usage_id_seq OWNED BY public.ai_slop_usage.id;
--
--
CREATE TABLE public.features (
    id integer NOT NULL,
    name character varying(255) NOT NULL,
    enabled boolean DEFAULT false NOT NULL
);
--
--
CREATE SEQUENCE public.features_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.features_id_seq OWNED BY public.features.id;
--
--
CREATE TABLE public.goku_poll_usage (
    id integer NOT NULL,
    user_id bigint NOT NULL,
    guild_id bigint NOT NULL,
    usage_count integer DEFAULT 0 NOT NULL,
    last_goku_at timestamp without time zone DEFAULT now() NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);
--
--
CREATE SEQUENCE public.goku_poll_usage_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.goku_poll_usage_id_seq OWNED BY public.goku_poll_usage.id;
--
--
CREATE TABLE public.gulag_users (
    id integer NOT NULL,
    user_id bigint NOT NULL,
    guild_id bigint NOT NULL,
    gulag_role_id bigint NOT NULL,
    channel_id bigint NOT NULL,
    in_gulag boolean NOT NULL,
    gulag_length integer NOT NULL,
    created_at timestamp without time zone NOT NULL,
    release_at timestamp without time zone NOT NULL,
    message_id bigint NOT NULL
);
--
--
CREATE SEQUENCE public.gulag_users_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.gulag_users_id_seq OWNED BY public.gulag_users.id;
--
--
CREATE TABLE public.gulag_votes (
    id integer NOT NULL,
    requester_id bigint NOT NULL,
    sender_id bigint NOT NULL,
    guild_id bigint NOT NULL,
    channel_id bigint NOT NULL,
    gulag_role_id bigint NOT NULL,
    processed boolean NOT NULL,
    message_id bigint NOT NULL,
    created_at timestamp without time zone NOT NULL
);
--
--
CREATE SEQUENCE public.gulag_votes_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.gulag_votes_id_seq OWNED BY public.gulag_votes.id;
--
--
CREATE TABLE public.is_this_real_usage (
    id integer NOT NULL,
    user_id bigint NOT NULL,
    guild_id bigint NOT NULL,
    last_used_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);
--
--
CREATE SEQUENCE public.is_this_real_usage_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.is_this_real_usage_id_seq OWNED BY public.is_this_real_usage.id;
--
--
CREATE TABLE public.message_votes (
    message_id bigint NOT NULL,
    channel_id bigint NOT NULL,
    guild_id bigint NOT NULL,
    user_id bigint NOT NULL,
    total_vote_tally integer CONSTRAINT message_votes_vote_tally_not_null NOT NULL,
    voters bigint[] NOT NULL,
    job_status public.job_status NOT NULL,
    current_vote_tally integer DEFAULT 0 NOT NULL
);
--
--
CREATE TABLE public.reversal_of_fortunes (
    user_id bigint NOT NULL,
    current_percentage bigint NOT NULL
);
--
--
CREATE TABLE public.servers (
    id integer NOT NULL,
    guild_id bigint NOT NULL,
    gulag_id bigint NOT NULL
);
--
--
CREATE SEQUENCE public.servers_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.servers_id_seq OWNED BY public.servers.id;
--
--
CREATE TABLE public.user_activity (
    user_id bigint NOT NULL,
    guild_id bigint NOT NULL,
    last_message_at timestamp without time zone NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);
--
--
ALTER TABLE ONLY public.ai_slop_usage ALTER COLUMN id SET DEFAULT nextval('public.ai_slop_usage_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.features ALTER COLUMN id SET DEFAULT nextval('public.features_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.goku_poll_usage ALTER COLUMN id SET DEFAULT nextval('public.goku_poll_usage_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.gulag_users ALTER COLUMN id SET DEFAULT nextval('public.gulag_users_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.gulag_votes ALTER COLUMN id SET DEFAULT nextval('public.gulag_votes_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.is_this_real_usage ALTER COLUMN id SET DEFAULT nextval('public.is_this_real_usage_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.servers ALTER COLUMN id SET DEFAULT nextval('public.servers_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.__diesel_schema_migrations
    ADD CONSTRAINT __diesel_schema_migrations_pkey PRIMARY KEY (version);
--
--
ALTER TABLE ONLY public.ai_slop_usage
    ADD CONSTRAINT ai_slop_usage_pkey PRIMARY KEY (id);
--
--
ALTER TABLE ONLY public.ai_slop_usage
    ADD CONSTRAINT ai_slop_usage_user_id_guild_id_key UNIQUE (user_id, guild_id);
--
--
ALTER TABLE ONLY public.features
    ADD CONSTRAINT features_name_key UNIQUE (name);
--
--
ALTER TABLE ONLY public.features
    ADD CONSTRAINT features_pkey PRIMARY KEY (id);
--
--
ALTER TABLE ONLY public.goku_poll_usage
    ADD CONSTRAINT goku_poll_usage_pkey PRIMARY KEY (id);
--
--
ALTER TABLE ONLY public.goku_poll_usage
    ADD CONSTRAINT goku_poll_usage_user_id_guild_id_key UNIQUE (user_id, guild_id);
--
--
ALTER TABLE ONLY public.gulag_users
    ADD CONSTRAINT gulag_users_pkey PRIMARY KEY (id);
--
--
ALTER TABLE ONLY public.gulag_votes
    ADD CONSTRAINT gulag_votes_pkey PRIMARY KEY (id);
--
--
ALTER TABLE ONLY public.is_this_real_usage
    ADD CONSTRAINT is_this_real_usage_pkey PRIMARY KEY (id);
--
--
ALTER TABLE ONLY public.is_this_real_usage
    ADD CONSTRAINT is_this_real_usage_user_id_guild_id_key UNIQUE (user_id, guild_id);
--
--
ALTER TABLE ONLY public.message_votes
    ADD CONSTRAINT message_votes_pkey PRIMARY KEY (message_id);
--
--
ALTER TABLE ONLY public.reversal_of_fortunes
    ADD CONSTRAINT reversal_of_fortunes_pkey PRIMARY KEY (user_id);
--
--
ALTER TABLE ONLY public.servers
    ADD CONSTRAINT servers_pkey PRIMARY KEY (id);
--
--
ALTER TABLE ONLY public.user_activity
    ADD CONSTRAINT user_activity_pkey PRIMARY KEY (user_id, guild_id);
--
--
CREATE INDEX idx_user_activity_guild_last_message ON public.user_activity USING btree (guild_id, last_message_at);
--
--

-- ---------------------------------------------------------------------------
-- Feature-flag seed data (seeded by the diesel migration history:
--   2026-02-10-190254-0000 insert_default_features
--   2026-03-12-000001 add_goku_poll_feature
--   2026-05-24-000001 add_is_this_real_feature
--   2026-06-13-200000 add_slow_user_auto_gulag_feature
--   2026-06-25-000001 add_cull_feature
-- Parity: the gulag/horny/phony/elon/derpies flag rows exist ONLY in the live
-- production DB (never in the migration history); on a fresh DB those
-- commands are disabled in BOTH Rust and Go, so they are not seeded here.
-- ---------------------------------------------------------------------------
INSERT INTO public.features (name, enabled) VALUES
    ('twitter', true),
    ('tiktok', true),
    ('instagram', true),
    ('bsky', true),
    ('ai_slop', true),
    ('teh', false);

INSERT INTO public.features (name, enabled) VALUES ('goku_poll', true)
ON CONFLICT (name) DO NOTHING;

INSERT INTO public.features (name, enabled) VALUES ('is_this_real', true)
ON CONFLICT (name) DO NOTHING;

INSERT INTO public.features (name, enabled) VALUES ('slow_user_auto_gulag', false)
ON CONFLICT (name) DO NOTHING;

INSERT INTO public.features (name, enabled) VALUES ('cull', false)
ON CONFLICT (name) DO NOTHING;
