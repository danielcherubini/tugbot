#!/usr/bin/env bash
# fake_pi — a minimal stand-in for `pi --mode rpc` (see PROTOCOL.md).
#
# Reads one JSON request line at a time on stdin and emits pi-shaped event
# lines on stdout. Ignores all command-line flags. Scenario knobs (env vars,
# inherited across the supervisor's auto-restart):
#
#   FAKE_PI_MESSAGE=str        response text (default "hello from fake pi");
#                              requests with an "images" field answer with
#                              "saw-image ..." instead
#   FAKE_PI_DIE=1              exit without responding on the FIRST run only
#                              (marker file $FAKE_PI_STATE_FILE gates it, so the
#                              supervisor's restart gets a live process)
#   FAKE_PI_DELAY=N            sleep N seconds between the ack and agent_end
#                              (exercises the ask timeout)
#   FAKE_PI_REJECT=1           answer the request with success:false
#   FAKE_PI_AGENT_END_FIRST=1  emit agent_end BEFORE the ack (abort #3)
#   FAKE_PI_AGENT_END_ERROR=1  emit agent_end with an error field (abort #4)
#   FAKE_PI_PID_FILE=path      write this process's PID to the file (Stop test)
#   FAKE_PI_STATE_FILE=path    "already ran" marker for FAKE_PI_DIE

if [[ -n "${FAKE_PI_PID_FILE:-}" ]]; then
  printf '%s\n' "$$" >"$FAKE_PI_PID_FILE"
fi

if [[ -n "${FAKE_PI_STATE_FILE:-}" ]]; then
  if [[ -n "${FAKE_PI_DIE:-}" && ! -f "$FAKE_PI_STATE_FILE" ]]; then
    touch "$FAKE_PI_STATE_FILE"
    echo "fake pi: dying (FAKE_PI_DIE=1, first run)" >&2
    exit 0
  fi
fi

i=0
while IFS= read -r line; do
  i=$((i + 1))
  id="$(printf '%s' "$line" | sed -E 's/.*"id"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/')"
  body="$(printf '%s' "$line" | sed -E 's/.*"message"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/')"

  if [[ "$line" == *'"images"'* ]]; then
    resp="saw-image (echo: ${body})"
  else
    resp="${FAKE_PI_MESSAGE:-hello from fake pi} (echo: ${body})"
  fi

  # Abort #3: agent_end before the prompt was accepted.
  if [[ -n "${FAKE_PI_AGENT_END_FIRST:-}" ]]; then
    printf '{"type":"agent_end"}\n'
  fi

  if [[ -n "${FAKE_PI_REJECT:-}" ]]; then
    # Abort #2: rejected request; the session itself is still alive.
    printf '{"id":"%s","success":false,"error":"fake rejection"}\n' "$id"
  else
    printf '{"id":"%s","success":true}\n' "$id"
  fi

  if [[ -n "${FAKE_PI_DELAY:-}" ]]; then
    echo "fake pi: delaying ${FAKE_PI_DELAY}s (request ${i})" >&2
    sleep "$FAKE_PI_DELAY"
  fi

  if [[ -n "${FAKE_PI_AGENT_END_ERROR:-}" ]]; then
    # Abort #4: agent_end after acceptance carrying an error field.
    printf '{"type":"agent_end","error":"fake agent error","messages":[]}\n'
    continue
  fi

  # Normal completion: minimal agent_end carrying the messages array.
  printf '{"type":"agent_end","messages":[{"role":"user","content":"%s"},{"role":"assistant","content":"%s"}]}\n' \
    "$body" "$resp"
done
