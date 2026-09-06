-- 000003_derpies_prompt — the derpies filter's LIVE prompt template.
--
-- Single-row table: body = the prompt template text (the template contract
-- — {content} / {known} markers, optional {{IMAGES}} / {{REF}} — lives in
-- code; an invalid row falls back to the code default, so a bad edit can
-- never leave the filter with a broken prompt). updated_at = last edit.
--
-- The seed body is the code default template, byte-for-byte. It contains ONE
-- single quote (the apostrophe in `filter's`) — the SQL literal doubles it:
-- `filter''s`. No other characters need escaping (no backticks, no
-- backslashes; semicolons inside the literal are safe — the whole file is
-- one simple-protocol query through pgx, the 000002 mechanics). Editing is an
-- operator UPDATE (no deploy, no restart — the next message picks it up);
-- rollback is restoring the previous body text.
CREATE TABLE public.derpies_prompt (
    id integer NOT NULL,
    body text NOT NULL,
    updated_at timestamp without time zone DEFAULT now() NOT NULL
);
--
--
CREATE SEQUENCE public.derpies_prompt_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
--
--
ALTER SEQUENCE public.derpies_prompt_id_seq OWNED BY public.derpies_prompt.id;
--
--
ALTER TABLE ONLY public.derpies_prompt ALTER COLUMN id SET DEFAULT nextval('public.derpies_prompt_id_seq'::regclass);
--
--
ALTER TABLE ONLY public.derpies_prompt
    ADD CONSTRAINT derpies_prompt_pkey PRIMARY KEY (id);
--
--
INSERT INTO public.derpies_prompt (body) VALUES
    ('A Discord message was just posted by a user with a documented history of spamming this server with a ROTATING ROSTER of short, repetitive, annoying gimmicks — and of evading, over and over, the word filters built to catch them. He is notorious for this.

HE WILL TEST THIS FILTER. Every message you judge from him is a probe: he actively measures what gets through, and the respellings in his posts are his evasions, not typos to forgive. Your stance is adversarial, not polite: when a message carries ANY trace of the roster — respelled, bent, squeezed, split, quoted, or dressed up as a question — judge it a GIMMICK. Judge CLEAN only when there is NO trace of the roster at all AND a plainly innocent reading is obvious. For this user a false negative (a gimmick getting through) is the worse error. When you are torn between the two: GIMMICK. His messages are the filter''s only queue, so err toward catching the roster, never toward letting it through.

{content}

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

{{IMAGES}}
{{REF}}

His gimmicks are short, repetitive solicitations he posts over and over. Example from the roster: trying to get other users to buy HIM a Zwift subscription, or to give him a free bicycle. The roster rotates — old gimmicks come back — so the known-word list below spans EVERY past gimmick, not just the current one.

Known gimmick words (each was the anchor word of a past gimmick; respellings of them are how he dodges the fast filter):
{known}

Judgement rules (these override politeness):
- A known word or any respelling of one — even when the surrounding text looks mildly innocent — is GIMMICK.
- A known word hidden inside another word, written in non-English letters, or shot full of punctuation and dashes is GIMMICK — dressing does not launder the word.
- An anchor word embedded inside a squeeze/blend is GIMMICK; the anchor word is the most distinctive token of the blend AS IT APPEARS.
- If you have to imagine an innocent reading to call it CLEAN, you are probably wrong — he is very good at making solicitations look like questions.
- When you are torn: GIMMICK.

Reply with EXACTLY one line, one of:
  GIMMICK:<word>
  CLEAN
where <word> is the anchor word: the as-appears respelled token for a known-gimmick trace, or the single most distinctive word of the fresh gimmick. The rules for <word>:
- It MUST be a token of the message text AS IT APPEARS (case and edge punctuation aside; ignore unicode bent — you SHOULD judge "žwift" to be "zwift").
- For a respelling, answer the respelled token AS IT APPEARS. NEVER answer the base/known word unless that base token itself appears in the message text — for "zwift" the answer is "zwift"; "GIMMICK:swift" for it is the INVALID answer. Never answer a known word that is not in the message.
- When the anchor word lives ONLY in an image, answer the most distinctive word of that image as if it were in the message.
- CLEAN only when the message carries NO trace of the roster at all and the innocent reading is obvious.');
