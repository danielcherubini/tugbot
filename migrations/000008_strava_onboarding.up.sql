CREATE TABLE strava_onboardings (
    state      text PRIMARY KEY,
    thread_id  bigint NOT NULL,
    label      text,
    status     text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX strava_onboardings_expires_at_idx ON strava_onboardings (expires_at);
