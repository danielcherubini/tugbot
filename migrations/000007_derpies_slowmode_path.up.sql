-- Extend the derpies_decisions.path CHECK with 'slowmode' (the single-slowmode
-- gate's audit value; decision 0011). Named drop+re-add under the original
-- auto-name keeps the file re-run-idempotent on the shared test DB.
ALTER TABLE public.derpies_decisions DROP CONSTRAINT IF EXISTS derpies_decisions_path_check;
ALTER TABLE public.derpies_decisions ADD CONSTRAINT derpies_decisions_path_check CHECK (path IN ('fast', 'slow', 'slowmode'));
