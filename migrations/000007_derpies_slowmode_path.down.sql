ALTER TABLE public.derpies_decisions DROP CONSTRAINT IF EXISTS derpies_decisions_path_check;
ALTER TABLE public.derpies_decisions ADD CONSTRAINT derpies_decisions_path_check CHECK (path IN ('fast', 'slow'));
