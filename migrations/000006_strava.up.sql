-- 000006_strava — schema for the strava feature (Go-origin).
-- DECLARED DEVIATION: the first timestamptz columns in a naive-timestamp DB
-- (only dbmigrate's own schema_migrations.applied_at predates this). Rationale:
-- these values come straight from the Strava epoch-seconds API and are
-- compared against now(), so tz-aware columns avoid local-time ambiguity.
-- All other tables are untouched.
CREATE TABLE public.strava_athletes (
    id               serial PRIMARY KEY,
    label            text NOT NULL,            -- display name in the post: "Matt"
    strava_athlete_id bigint NOT NULL UNIQUE,  -- Strava's id for the user
    access_token     text NOT NULL,
    refresh_token    text NOT NULL,            -- ROTATES on every refresh — must be re-persisted
    token_expires_at timestamptz NOT NULL,
    last_polled_at   timestamptz,             -- the `after` cursor; NULL = first poll = 24h lookback
    target_thread_id bigint,                 -- nullable: per-athlete thread, else shared fallback
    needs_reauth     boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.strava_seen_activities (
    id                 serial PRIMARY KEY,
    strava_athletes_id int NOT NULL REFERENCES public.strava_athletes(id) ON DELETE CASCADE,
    strava_activity_id bigint NOT NULL,
    start_date         timestamptz NOT NULL,  -- the activity's start time; watermark math from here
    status             text NOT NULL CHECK (status IN ('pending', 'posted', 'skipped')),
    retries            int NOT NULL DEFAULT 0,  -- full cycles the 'pending' row has been re-fetched (drop at 5; 0 on its discovery pass)
    dispositioned_at   timestamptz NOT NULL DEFAULT now(),  -- when the row was first created (any status)
    UNIQUE (strava_athletes_id, strava_activity_id)  -- crash-safe dedupe key
);

INSERT INTO public.features (name, enabled) VALUES ('strava', false) ON CONFLICT (name) DO NOTHING;
