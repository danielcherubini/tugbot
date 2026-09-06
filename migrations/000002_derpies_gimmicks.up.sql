-- 000002_derpies_gimmicks — the derpies filter's gimmick word list + flag row.
--
-- derpies_gimmicks: words that signal Derpie's recurring respelling scheme.
-- `source` distinguishes 'seed' (this file) from 'llm' (learnt at runtime
-- by the handler's pi RPC verdict). The UNIQUE word constraint makes the
-- runtime insert an idempotent upsert.
--
-- The `features` insert is ON CONFLICT DO NOTHING: the live production DB
-- already carries a 'derpies' flag row (the baseline migration's comment);
-- on a fresh DB this seeds the feature OFF.
CREATE TABLE public.derpies_gimmicks (
    id integer NOT NULL,
    word character varying(64) NOT NULL,
    source character varying(8) DEFAULT 'seed' NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);
--
--
CREATE SEQUENCE public.derpies_gimmicks_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.derpies_gimmicks_id_seq OWNED BY public.derpies_gimmicks.id;
--
--
ALTER TABLE ONLY public.derpies_gimmicks ALTER COLUMN id SET DEFAULT nextval('public.derpies_gimmicks_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.derpies_gimmicks
    ADD CONSTRAINT derpies_gimmicks_pkey PRIMARY KEY (id);
--
--
ALTER TABLE ONLY public.derpies_gimmicks
    ADD CONSTRAINT derpies_gimmicks_word_key UNIQUE (word);
--
--
INSERT INTO public.derpies_gimmicks (word, source) VALUES
    ('swift', 'seed'),
    ('zswift', 'seed'),
    ('bike', 'seed'),
    ('give', 'seed'),
    ('buy', 'seed');
--
--
INSERT INTO public.features (name, enabled) VALUES ('derpies', false)
ON CONFLICT (name) DO NOTHING;
