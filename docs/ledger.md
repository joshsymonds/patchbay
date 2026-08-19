# Ledger

Every proxied request appends one JSON object, on one line, to a file named for
the UTC day it happened:

```
$PATCHBAY_LEDGER_DIR/patchbay-2026-08-19.jsonl
```

The directory is created 0700 and files are 0600 — the records name every model
a machine used and when, which is nobody else's business. Rotation is nothing
but the choice of filename at append time; no timer runs. The directory is held
as an `os.Root` handle, so every file operation is resolved inside it and cannot
escape.

Writes happen off the request path: the caller is answered first, then the
record is built and appended by a separate goroutine. Each append is written,
`fsync`ed and closed before it is reported as written. A ledger that fails —
unwritable directory, full disk, a record that fails validation — costs the
record and logs a warning; it never costs the request. At shutdown, in-flight
records get a bounded 5 s to finish before the ledger is closed.

## Record schema

Real records, from a live gateway:

```json
{"timestamp":"2026-08-19T00:22:56.18060013-07:00","host":"workstation","context":"personal","credential_class":"none","alias":"stub/kimi","upstream":"http://127.0.0.1:4300","model":"moonshotai/kimi-k2","request_model":"stub/kimi","auth_mode":"inject","status":200,"input_tokens":12,"output_tokens":5,"duration_ms":0,"upstream_ms":0,"block_shape":["text"],"drop_signature":false,"stop_reason":"end_turn"}
{"timestamp":"2026-08-19T00:22:56.186040329-07:00","host":"workstation","context":"personal","credential_class":"none","alias":"stub/kimi","upstream":"http://127.0.0.1:4300","model":"moonshotai/kimi-k2","request_model":"stub/kimi","auth_mode":"inject","status":403,"input_tokens":0,"output_tokens":0,"duration_ms":0,"upstream_ms":0,"block_shape":null,"drop_signature":false,"error_body":"{\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"patchbay: caller key missing or mismatched\"}}"}
```

| Field | Type | Meaning |
|---|---|---|
| `timestamp` | RFC 3339 | When the request arrived, in local time with offset. The UTC date of this value chooses the file. |
| `host` | string | The machine's hostname. Required, non-blank. |
| `context` | string | The context that served the request — `personal` for every request that named none. |
| `credential_class` | string | What kind of credential the **client** presented: `oauth` (an `Authorization: Bearer …`, which is what a Claude Code subscription session sends), `api_key` (an `x-api-key` header), or `none`. Never which credential. |
| `alias` | string | The registry key the route was found under. Empty for a default route, built-in or declared. |
| `upstream` | string | Where the request went: the route's `base_url`, or for a `sigv4` route the `bedrock-runtime` endpoint its region derives. Required, non-blank. |
| `model` | string | The model actually asked of the upstream: the route's rewrite target, or the model the caller named when the route rewrites nothing. `(none)` when the request named none. |
| `request_model` | string | The model the caller asked for, before any rewrite. `(none)` when the request named none. |
| `auth_mode` | string | `forward`, `inject` or `sigv4`. Anything else is refused. |
| `status` | int | The status the client received. For a request patchbay refused itself, that refusal's status. |
| `input_tokens` | int | From the response. Zero when there was no summary to take it from. |
| `output_tokens` | int | From the response — for a stream, the last figure reported, since a stream revises the count as it goes. |
| `duration_ms` | int | Whole milliseconds from arrival to the end of the response stream. |
| `upstream_ms` | int | Whole milliseconds from sending the upstream request to receiving its headers. |
| `block_shape` | array of string \| null | The ordered content-block types of the response: `["text"]`, `["thinking","text","tool_use"]`, and so on. `null` when there was nothing to summarize. |
| `drop_signature` | bool | True when `block_shape` contains two adjacent reasoning blocks (`thinking` or `redacted_thinking`, in any combination) — a block boundary with nothing user-visible between them, which is the mid-turn text drop signature. |
| `stop_reason` | string, omitted when empty | The response's stop reason. |
| `error_body` | string, omitted when empty | For a failure patchbay originated, the Anthropic-shaped error body it sent. For a non-2xx from upstream, the upstream's own captured body. |
| `request_body` | string, omitted when empty | The exact bytes forwarded upstream. Only for routes with `capture_bodies: true`. |
| `response_body` | string, omitted when empty | The captured response, whatever the status. Only for routes with `capture_bodies: true` — and on a non-2xx the same bytes also appear, truncated, in `error_body`. |

The summary fields — `input_tokens`, `output_tokens`, `block_shape`,
`stop_reason` — are filled in only for a 2xx with a usable capture. On any other
status the captured body is recorded as `error_body` instead and the summary
fields stay at their zero values, because a body that is not a completed turn
has no token accounting worth reporting.

### No auth material, by construction

The schema has no field for headers of any kind. Request and response headers
carry `Authorization` and `x-api-key`, so there is nowhere in a record to put
them — this is a missing field, not a rule someone has to remember.

`credential_class` is the one attribution field that touches credentials, and it
is a closed vocabulary of three constants written by a classifier that reads
only whether a header is present and whether it starts with `bearer`. No header
value can become one of those three, so no value can leak through it. Validation
refuses any other value outright.

Error bodies that patchbay writes never echo a presented or expected key: the
caller-key refusal above says "missing or mismatched" and nothing more, and an
AWS credential-export failure names only the profile.

## Validation

`Validate` runs before anything is written, and again on every line that is read
back, so a malformed record never reaches a file and one that somehow did is
reported as corruption rather than returned as if it were sound. A record must
have:

- non-blank `host`, `upstream`, `model` and `request_model`;
- `auth_mode` in {`forward`, `inject`, `sigv4`};
- `credential_class` in {`oauth`, `api_key`, `none`} — or empty, see below;
- `context` empty, or lowercase letters, digits and dashes only;
- a non-zero `timestamp`;
- non-negative `status`, token counts and durations.

`(none)` is the stand-in the pipeline writes for a model the request never gave,
precisely so a request that named nothing is recorded as exactly that rather
than dropped by the non-blank rule.

### Legacy records

`context` and `credential_class` are accepted when **absent** on read. Records
written before contexts existed carry neither field, and a ledger that refused
to read its own history would be a worse failure than a missing field. The
leniency is read-side only: the pipeline always populates both, so nothing new
is written that shape. A `context` that *is* present is held to the shape a
context name has, so a value that could only have come from somewhere other than
a resolved context is refused rather than stored.

## Truncation and capture

Three different things can shorten what a record holds, and they are
deliberately distinguishable:

- **`error_body` truncation.** Capped at 4096 bytes at append time, cut back to
  a UTF-8 rune boundary so a stored line never contains a split rune. A runaway
  upstream must not be able to make one ledger line unbounded.
- **Capture dropped at the ceiling.** The response tee holds at most 64 MiB. Past
  that the whole capture is dropped rather than kept as a prefix, and a warning
  is logged. The record is still written — with no `block_shape`, no token
  counts and no stop reason — because half a stream reported as if it were whole
  is worse than no summary.
- **Stream truncated mid-flight.** If the client goes away mid-stream, or the
  upstream's body ends with a non-EOF error before its own `message_stop`, the
  capture is marked truncated and yields nothing, for the same reason. A warning
  is logged.

Summarization itself never fails: an unparsable, empty or partial body yields an
empty summary, and the record is written either way. Token counts, block shapes
and stop reasons are read from both response shapes — a complete JSON message,
or an SSE stream walked event by event (`message_start` for input tokens,
`content_block_start` for the shape, `message_delta` for output tokens and the
stop reason). For a Bedrock route it is the translated SSE that is summarized,
because that is what the caller actually received.

## Reading records

They are JSONL: `jq` works, and so does anything else that reads a line at a
time.

```console
$ jq -r 'select(.status != 200) | [.timestamp, .context, .alias, .status] | @tsv' \
    ~/.local/state/patchbay/ledger/patchbay-2026-08-19.jsonl

$ jq -s 'group_by(.context) | map({context: .[0].context, requests: length,
      input: (map(.input_tokens) | add), output: (map(.output_tokens) | add)})' \
    ~/.local/state/patchbay/ledger/*.jsonl

$ jq -r 'select(.drop_signature) | [.timestamp, .model, (.block_shape | join(","))] | @tsv' \
    ~/.local/state/patchbay/ledger/*.jsonl
```

patchbay's own reader (`ledger.Scan`, used by the tests) stops at the first line
that will not parse or will not validate, returning the records before it
alongside a `CorruptionError` naming the byte offset — damage is surfaced, never
silently skipped. A final line missing its newline counts as corruption, since a
half-written record is not a record.
