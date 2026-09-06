-- 000004_derpies_cog_words — add the user's "cog" respellings to the gimmick list.
--
-- Derpie's own posts use "cog"-style spellings; seed them like the 000002
-- words so the fast path hits exact tokens. All lowercase alnum (the
-- wordValid charset), source='seed' like the original five.
INSERT INTO public.derpies_gimmicks (word, source) VALUES
    ('cog', 'seed'),
    ('cogs', 'seed'),
    ('coggs', 'seed'),
    ('c0g', 'seed'),
    ('c0gs', 'seed'),
    ('coq', 'seed'),
    ('coqs', 'seed'),
    ('kog', 'seed'),
    ('kogs', 'seed');