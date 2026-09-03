# `pi --mode rpc` wire protocol (tugbot <-> pi)

One stdin JSONL request per turn; the subprocess replies on stdout with one-line
JSON objects. The supervisor (this package) owns the subprocess and restarts it
on crash.

- **Request** (stdin, one JSON object per line):
  `{"id":"req-N","type":"prompt","message":"<prompt>"}` — plus, only when
  images are present: `"images":[{"type":"image","data":"<base64>","mimeType":"<mime>"}, ...]`.
- **Ack** (stdout, first response line for a request, carries `id`):
  `{"id":"req-N","success":true}` on acceptance, or
  `{"id":"req-N","success":false,"error":"..."}` on rejection (abort #2: error
  immediately, no restart).
- **Events** (stdout, lines without an `id` field): `{"type":"<event_name>",...}`.
  Only `agent_end` ends a turn; other event types are ignored.
- **Completion**: `agent_end` after its ack carries the conversation
  `messages` array; the answer is the text of the LAST message with
  `role:"assistant"` — `content` is either a string or a block array
  (`[{"type":"text","text":...}]`, non-text blocks like `tool_use` are skipped).
- **Abort conditions** (mirror `src/pi_rpc.rs`): (1) EOF on stdout → sentinel
  error, subprocess marked dead, restarted before the next request; (2) request
  ack `success:false` → error, no restart; (3) `agent_end` before its ack was
  accepted → error, no restart; (4) `agent_end` after acceptance with an
  `error` field → error, no restart.
- **Timing**: asks time out at 300 s; spawn/restart paths sleep 200 ms to let
  the subprocess initialize. `tugbot-system-prompt.md` and the hardcoded
  SECURITY guardrail are both appended via `--append-system-prompt`.
