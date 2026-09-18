-- 000005_derpies_score — the derpies score model: the live dials, the
-- decision log, and the phrase optimizer, plus the prompt flip.
--
-- derpies_config (single row — the operator's live dials): the delete
-- threshold T the decision matrix reads per message (learn at score >=
-- 40, delete at score >= T). The CHECK (BETWEEN 41 AND 100) keeps T >
-- the learn floor (the matrix is defined for T > 40 only); the code
-- clamps a pre-CHECK read to the default 50.
--
-- derpies_decisions (append-only — one row per judged message/edit):
-- the full decision record (the path, the score + threshold, the learned
-- word, the learn/delete outcomes, the rejection reason). NULL path =
-- the arm never reached a path (pre-matrix degradation arms); NULL
-- score/threshold = the arm never reached the matrix.
--
-- derpies_gimmick_phrases (manual-only — the optimizer): curated
-- multi-word patterns the fast path matches as an EXACT consecutive run
-- of the folded token sequence (zero pi asks). source is 'manual' —
-- the LLM never writes here.
--
-- The prompt flip (atomic with the DDL): the live derpies_prompt row is
-- set to the SCORE-format prompt in the same deploy, so the code is
-- never in a "new prompt + old parser" dead state. The literal is the
-- code default template byte-for-byte with the two apostrophes
-- ('filter's' / 'message's') SQL-doubled.
CREATE TABLE public.derpies_config (
    id integer NOT NULL,
    delete_threshold integer DEFAULT 50 NOT NULL,
    -- updated_at is NOT auto-updated (no trigger): an operator UPDATE of
    -- delete_threshold should also set updated_at = now().
    updated_at timestamp without time zone DEFAULT now() NOT NULL
);
CREATE SEQUENCE public.derpies_config_id_seq AS integer START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1;
ALTER SEQUENCE public.derpies_config_id_seq OWNED BY public.derpies_config.id;
ALTER TABLE ONLY public.derpies_config ALTER COLUMN id SET DEFAULT nextval('public.derpies_config_id_seq'::regclass);
ALTER TABLE ONLY public.derpies_config ADD CONSTRAINT derpies_config_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.derpies_config ADD CONSTRAINT derpies_config_threshold_check CHECK (delete_threshold BETWEEN 41 AND 100);
-- Enforce the singleton: the config is a single row (id = 1). A second
-- insert is rejected, so the runtime `SELECT ... WHERE id = 1` can never
-- pick an arbitrary row.
ALTER TABLE ONLY public.derpies_config ADD CONSTRAINT derpies_config_singleton_check CHECK (id = 1);
INSERT INTO public.derpies_config (id, delete_threshold) VALUES (1, 50);

-- derpies_decisions (append-only — one row per judged message/edit)
CREATE TABLE public.derpies_decisions (
    id bigserial PRIMARY KEY,
    message_id text NOT NULL,
    channel_id text NOT NULL,
    author_id text NOT NULL,
    content text NOT NULL DEFAULT '',
    path text CHECK (path IN ('fast', 'slow')),
    score integer,
    threshold integer,
    word text,
    learned boolean NOT NULL DEFAULT false,
    deleted boolean NOT NULL DEFAULT false,
    reject_reason text,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);
CREATE INDEX derpies_decisions_author_created_idx ON public.derpies_decisions (author_id, created_at DESC);
CREATE INDEX derpies_decisions_created_idx ON public.derpies_decisions (created_at DESC);

-- derpies_gimmick_phrases (manual-only — the optimizer)
CREATE TABLE public.derpies_gimmick_phrases (
    id integer NOT NULL,
    phrase character varying(300) NOT NULL,
    source character varying(8) DEFAULT 'manual' NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);
CREATE SEQUENCE public.derpies_gimmick_phrases_id_seq AS integer START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1;
ALTER SEQUENCE public.derpies_gimmick_phrases_id_seq OWNED BY public.derpies_gimmick_phrases.id;
ALTER TABLE ONLY public.derpies_gimmick_phrases ALTER COLUMN id SET DEFAULT nextval('public.derpies_gimmick_phrases_id_seq'::regclass);
ALTER TABLE ONLY public.derpies_gimmick_phrases ADD CONSTRAINT derpies_gimmick_phrases_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.derpies_gimmick_phrases ADD CONSTRAINT derpies_gimmick_phrases_phrase_key UNIQUE (phrase);

-- The prompt flip (atomic with the DDL).
UPDATE public.derpies_prompt SET body = 'A Discord message was just posted by a user with a documented history of spamming this server with a ROTATING ROSTER of short, repetitive, annoying gimmicks — and of evading, over and over, the word filters built to catch them. He is notorious for this.

HE WILL TEST THIS FILTER. Every message you judge from him is a probe: he actively measures what gets through, and the respellings in his posts are his evasions, not typos to forgive. Your stance is adversarial, not polite: when a message carries ANY trace of the roster — respelled, bent, squeezed, split, quoted, or dressed up as a question — judge it a GIMMICK. Judge it innocent only when there is NO trace of the roster at all AND a plainly innocent reading is obvious. For this user a false negative (a gimmick getting through) is the worse error. When you are torn between two bands: score toward the HIGHER side. His messages are the filter''s only queue, so err toward catching the roster, never toward letting it through.

{content}
{{EMBED}}
{{GIFS}}

Techniques he uses — in any combination; judge on ALL of them at once:
- RESPPELLING: letters swapped/added/dropped/reordered, or bent — including unicode lookalikes (a z or s with a diacritic, ß, ø, ς, and the like), all-caps, or letters spelled out. Examples: zwift, schwift, žwift, s1ft. A bent letter does NOT change the word: "žwift" IS the swift-thing.
- NON-ENGLISH LETTERS: a known word written in Cyrillic, Greek, or any other lookalike script (з = z, и = i, о = o, ο = o, ς = s, and the like) IS that known word. Judge by what it spells, not by which script it is wearing.
- WEIRD SPELLINGS OF EVERY KIND: any spelling of a known word that a reasonable reader can still see through — letter transpositions, doubled letters, "wrong" but recognizable spellings. If it is recognizably the known word, judge it.
- PUNCTUATION / DASHES EVERYWHERE: punctuation, dashes, dots, slashes, brackets, or symbols wedged INTO a known word (sw-ift, s.w.i.f.t, s/w/i/f/t, s(w)i(f)t), or between its letters — punctuation does not break the word.
- SPLIT: a known word spread over spaces or symbols between its letters (g i v e, s w i f t with dots/dashes between the letters).
- HIDDEN IN OTHER WORDS: a known word buried inside a longer word it is not a token of (a "swift"-like string stitched into another word, a known word straddling a word boundary, or known words jammed together into one token) — it still counts; the anchor is the token containing it, AS IT APPEARS.
- SQUEEZED/CONCATENATED: a known word fused into or onto another word without the space (a "swiftin…"-style blend), one or more known words jammed together, or extra letters sprinkled through a known word.
- ASK-PHRASING (the core of the roster): asking OTHER users to buy/give him something — a Zwift subscription, a free bicycle, a "gift" keyed to a known word — OR a fresh short repetitive solicitation in the same style (a FRESH gimmick in the roster style counts).
- QUOTING/REFERENCING: replying to or quoting one of his own earlier messages so the gimmick lives in the quote (quoted text counts as part of the message).
- IMAGES: the gimmick inside an attached/quoted screenshot or pasted image (images arrive with the message for you to read; a word visible in an image counts as if it were written).
- EMOJI-ENCODING: a sequence of emojis whose COMBINED meaning is a roster solicitation (💸 + 🚵 = "buy me a bike" = zwift; 💰 + a face + 🚲 = "buy me a bike"). Judge the COMBINATION, not the individual emojis — a single emoji (money, a bike, a face) is harmless alone; the combo is the gimmick. A combo that plainly means a roster solicitation scores 90-100.

{{IMAGES}}
{{REF}}

His gimmicks are short, repetitive solicitations he posts over and over. Example from the roster: trying to get other users to buy HIM a Zwift subscription, or to give him a free bicycle. The roster rotates — old gimmicks come back — so the known-word list below spans EVERY past gimmick, not just the current one.

Scoring scale (score the WHOLE message, all techniques at once):
- 90-100: an unambiguous roster solicitation — a known word as-is (any script), or a combo (emoji/image/text) that plainly means one.
- 60-89: a clear trace — a recognizable respelling / squeeze / split / foreign-script rendering of a known word, or a solicitation phrasing in the roster style.
- 40-59: a possible trace — a bent letter, a partial pattern, a combo that could go either way.
- 0-39: no meaningful trace — a plainly innocent reading.

Known gimmick words (each was the anchor word of a past gimmick; respellings of them are how he dodges the fast filter):
{known}

Judgement rules (these override politeness):
- A known word or any respelling of one — even when the surrounding text looks mildly innocent — is a GIMMICK (score it 60-100 by the scale).
- A known word hidden inside another word, written in non-English letters, or shot full of punctuation and dashes is a GIMMICK — dressing does not launder the word.
- A KNOWN GIMMICK IN ANY LANGUAGE IS STILL A GIMMICK: he now posts the same roster in OTHER LANGUAGES (observed: Arabic دراجة زويفت / زويفت, Mandarin 骑行/飞快/长城, Persian دوچرخه). The roster is the MEANING — a message that asks someone to buy/give him a bicycle, a Zwift subscription, or riding gear, in any script, language, or wording, is a GIMMICK. Translate the message in your head and judge what it MEANS, never let the script launder it.
- An anchor word embedded inside a squeeze/blend is a GIMMICK; the anchor word is the most distinctive token of the blend AS IT APPEARS.
- If you have to imagine an innocent reading to score it 0-39, you are probably wrong — he is very good at making solicitations look like questions.
- When you are torn between two bands: score toward the HIGHER side.

Reply with one or two lines — the SCORE line always, the WORD line only when
the message carries a real trace (score >= 40):
  SCORE:<0-100>
  WORD:<anchor>
where <word> is the anchor word: the as-appears respelled token for a known-gimmick trace, or the single most distinctive word of the fresh gimmick. The rules for <word>:
- It MUST be a token of the message text AS IT APPEARS (case and edge punctuation aside; ignore unicode bent — you SHOULD judge "žwift" to be "zwift") — EXCEPT when the gimmick lives ONLY in the emojis (the message has no other text words): then answer the most distinctive word of what the emojis MEAN (e.g. "zwift" for 💸🚵).
- When the anchor is in a NON-LATIN script, answer the message''s OWN foreign-script token as it appears (e.g. زويفت, دراجة, 骑行, دوچرخه) — NEVER the English-known-word translation unless that English word literally appears in the message. "zwift" for a message containing only زويفت is the INVALID answer; "زويفت" is correct.
- For a respelling, answer the respelled token AS IT APPEARS. NEVER answer the base/known word unless that base token itself appears in the message text — for "zwift" the answer is "zwift"; "swift" for it is the INVALID answer. Never answer a known word that is not in the message. The same rule holds across scripts: a foreign-script rendering of a known word is answered by its OWN script token, never by the English base.
- For a SPLIT word (letters spread over spaces or symbols between its letters), answer the COLLAPSED form — the letters joined without the spacing: "z w i f t" -> "zwift", "g i v e" -> "give". Never the spaced form; the spaced form is not a valid answer.
- When the anchor word lives ONLY in an image, answer the most distinctive word of that image as if it were in the message.
- When the anchor word lives ONLY in the emojis (the message has no other text words), answer the most distinctive word of what the emojis MEAN (e.g. "zwift" for 💸🚵) — not a token of the message.
- Score 0-39 only when the message carries NO trace of the roster at all and the innocent reading is obvious.', updated_at = now();
