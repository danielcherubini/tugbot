-- Restore the two-value path CHECK — the manual-rollback artifact for
-- 000007 (decision 0011); the db runner globs *.up.sql only, so this
-- file is executed directly (the DB-gated migration test does so via
-- pool.Exec).
ALTER TABLE public.derpies_decisions DROP CONSTRAINT IF EXISTS derpies_decisions_path_check;
ALTER TABLE public.derpies_decisions ADD CONSTRAINT derpies_decisions_path_check CHECK (path IN ('fast', 'slow'));
