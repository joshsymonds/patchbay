# Ledger v2

Every request that enters the proxy pipeline is ledger-eligible, including
registry, Context, body, rewrite-target, caller-key, and selected-Seat failures.
Model and introspection endpoints remain unledgered.

Records are one JSON object per line:

```text
$PATCHBAY_LEDGER_DIR/patchbay-v2-YYYY-MM-DD.jsonl
```

The filename uses the record timestamp's UTC date. Files rotate on append when
the date changes. Patchbay does not enforce size or retention limits and does not
delete old files.

The ledger directory is created with mode 0700 and new files with mode 0600.
Existing permissions are not changed. Each complete append is followed by
`fsync`.

Writes happen in a goroutine after the client response. A validation, open, write,
sync, or close failure drops the record and logs `ledger record dropped`; it does
not change the HTTP response. At shutdown, pending writes get up to five seconds
after HTTP shutdown.

Only `patchbay-v2-*.jsonl` files are scanned. There is no v1 decoder and no legacy
optional-field compatibility. A legacy-shaped line placed in a v2 file is
corruption.

## Selected record

```json
{
  "timestamp": "2026-08-20T18:00:00-07:00",
  "host": "workstation",
  "context": "personal",
  "selection": {
    "kind": "model",
    "model": "openrouter/kimi-k2"
  },
  "seat": "openrouter-kimi-k2",
  "upstream": "https://openrouter.ai/api",
  "model": "moonshotai/kimi-k2",
  "request_model": "openrouter/kimi-k2",
  "auth_mode": "inject",
  "credential_class": "none",
  "config_source": "PATCHBAY_OPENROUTER_KEY_FILE",
  "status": 200,
  "input_tokens": 12,
  "output_tokens": 5,
  "duration_ms": 320,
  "upstream_ms": 40,
  "block_shape": ["text"],
  "drop_signature": false,
  "stop_reason": "end_turn"
}
```

A selected record requires all routing fields:

| Field | Meaning |
|---|---|
| `timestamp` | Request start time. Its UTC date selects the file. |
| `host` | Gateway hostname. |
| `context` | Context that resolved the request. |
| `selection` | Why the Seat was selected. |
| `seat` | Private global Seat ID. |
| `subagent` | Optional positively validated direct agent observation. |
| `upstream` | Effective Seat upstream. It does not prove the request reached it. |
| `model` | Seat rewrite target, or the requested model when the Seat rewrites none; JSON `null` when both are exactly absent. Always present on a selected record. SigV4 stores the map key, never the mapped Bedrock identifier. |
| `request_model` | Requested model before rewriting; JSON `null` when the request named none at all. Always present on a selected record. |
| `auth_mode` | `forward`, `inject`, or `sigv4`. |
| `credential_class` | Inbound credential shape: `oauth`, `api_key`, or `none`. |
| `config_source` | Non-secret configured credential source. |
| `status` | Status returned to the client. |
| `input_tokens`, `output_tokens` | Counts parsed from a successful response, otherwise zero. |
| `duration_ms` | Whole milliseconds from request arrival through response copy or gateway error. |
| `upstream_ms` | Whole milliseconds until upstream response headers, or zero. |
| `block_shape` | Ordered content-block types; `null` when no retained response was summarized. |
| `drop_signature` | True when adjacent reasoning blocks appear in `block_shape`. |
| `stop_reason` | Parsed stop reason, omitted when empty. |
| `error_body` | Gateway error or non-2xx upstream response, omitted when empty. |
| `request_body`, `response_body` | Bodies stored only when `capture_bodies` is enabled and capture succeeds. |

`model` and `request_model` hold opaque exact strings or JSON `null`. A model
selector is matched byte for byte and never trimmed, folded or otherwise
normalized, so one made of nothing but whitespace, or one an operator named
`(none)`, is a selector like any other and is stored verbatim.

No string is reserved to mean absence. Absence is `null`, which is not a string,
so it collides with no selector:

```json
{"model": null, "request_model": null}
{"model": "(none)", "request_model": "(none)"}
```

The first is a request that named no model on a Seat that rewrites none. The
second is a Seat bound to a selector spelled `(none)`. They are different records
and different lines.

On a selected record both keys are required and always emitted, so a line
missing either one is corruption rather than a model nobody named. A
pre-selection record carries no `model` key at all, and spells an absent
`request_model` by omitting it — an explicit `null` there is corruption too, so
each state has exactly one spelling and every record round-trips as itself.

`config_source` is exactly one of these non-secret descriptor forms:

```text
caller
<API-key environment-variable name>
aws_profile:<name>
```

It never contains a credential or resolved file path.

## Selection

`selection.kind` is closed to:

| Kind | `selection.model` |
|---|---|
| `model` | Exact matched public selector. |
| `agent_type` | JSON `null`; `selection.agent_type` names the agent type matched in `context.subagents.agent_types`. |
| `subagent` | Exact matched requested model from `context.subagents.models`, or JSON `null` when marked traffic fell to `context.subagents.default_seat` naming no model. |
| `default` | JSON `null`, for the ordinary Context default. |

`selection.model` is the only thing separating an exact marked hit from the
unnamed marked fallback, so validation requires a non-empty model for `model`,
allows either a non-empty model or `null` for `subagent`, and requires `null`
for `agent_type` and `default`. `selection.agent_type` is required and non-empty
for `agent_type` and absent for every other kind.

A marked-subagent exact record may include:

```json
{
  "selection": {
    "kind": "subagent",
    "model": "claude-opus-5"
  },
  "subagent": {
    "agent_id": "agent-aefe5b1a70030c259",
    "parent_agent_id": "agent-parent-id"
  }
}
```

An agent-type record names the binding instead, and the observation carries the
reported type:

```json
{
  "selection": {
    "kind": "agent_type",
    "model": null,
    "agent_type": "Explore"
  },
  "subagent": {
    "agent_id": "agent-aefe5b1a70030c259",
    "agent_type": "Explore"
  }
}
```

Only positively validated values enter the ledger. Agent ID is required for the
nested object; parent ID and agent type are optional. Each must be 1–256 bytes and contain only
printable ASCII bytes `0x21`–`0x7e`. Parent-only or invalid observations are
omitted.

These IDs are correlation observations. They are not authenticated identity,
authorization, permission, or a boundary signal.

## Pre-selection failures

A request refused before Seat selection writes a reduced record:

```json
{
  "timestamp": "2026-08-20T18:01:00-07:00",
  "host": "workstation",
  "context": "missing",
  "credential_class": "oauth",
  "status": 404,
  "input_tokens": 0,
  "output_tokens": 0,
  "duration_ms": 0,
  "upstream_ms": 0,
  "block_shape": null,
  "drop_signature": false,
  "error_body": "{...}"
}
```

Pre-selection records:

- retain the requested Context when it is a bounded printable name — 1–256
  bytes of ASCII `0x21`–`0x7e`. A requested Context is a caller-supplied path
  segment of any length carrying any bytes, so one the field cannot represent is
  omitted while the request itself is still recorded;
- retain request model only when it was successfully parsed before refusal, as
  an exact string; omit the key otherwise;
- retain validated subagent observation;
- retain inbound `credential_class`;
- omit `selection`, `seat`, `upstream`, `model`, `auth_mode`, and
  `config_source`.

A body-read failure has no parsed request model. Registry-invalid and
unknown-Context failures occur before body parsing. A protected rewrite-target
refusal retains the parsed `request_model` but still has no selected Seat.

After Seat selection, failures retain the full selected routing fields. This
includes rewrite/body transformation errors, caller-key refusal, provider-key or
model-map failure, AWS credential export failure, upstream connection failure,
and non-2xx upstream responses.

## Credential handling

The schema has no request-header or response-header fields. Authorization,
API-key, caller-key, SigV4, and AWS session-token headers therefore have no place
in a record.

`credential_class` records only inbound header shape:

1. Bearer `Authorization` — `oauth`;
2. otherwise a non-empty `X-Api-Key` — `api_key`;
3. otherwise — `none`.

Captured bodies may contain sensitive application data. Enable `capture_bodies`
only when the ledger's storage and retention are suitable.

For SigV4, `request_body` is captured after common Seat rewrites but before
Bedrock removes `model` and `stream` and adds `anthropic_version`. Bedrock response
streams are captured after translation to Anthropic SSE.

On a non-2xx upstream response, retained bytes become `error_body`. With body
capture enabled, the same bytes also remain in `response_body`.

## Validation

Every appended or scanned v2 record is validated. Selected records require a
valid Context, selection, Seat, upstream, auth mode, credential class, and
configured source, and both model keys. A model is either absent or an exact
non-empty string; an empty string is refused in either field, on either record
shape, because absence already has a spelling. Pre-selection records must omit
every selection-dependent field, `model` included. Timestamp and host are
required; status, token counts, and durations must be non-negative.

`Ledger.Scan` rejects three things about the raw shape of a v2 line: unknown
fields, at every depth; trailing data after the record; and duplicate JSON member
names, recursively, in every object at every depth, including objects carried in
arrays. A repeated name is refused rather than resolved last-wins, because a
decoder keeps only the second value and leaves nothing of the first.

## Capture limits

`error_body` is truncated to 4096 bytes on a UTF-8 boundary.

The response summary capture is limited to 64 MiB. If a response exceeds that
limit, the complete capture is discarded rather than stored as a prefix. The
client still receives the response, and the record is attempted without summary
or response body. Request bodies have their own 64 MiB proxy limit.

A client-write failure, upstream read error, or Bedrock stream decoding error also
discards response capture. A clean EOF is not checked for an Anthropic
`message_stop` event; a clean prefix may still be summarized.

Only retained 2xx responses are summarized. JSON responses provide block types,
usage, and stop reason. SSE responses provide those values from `message_start`,
`content_block_start`, and `message_delta` events.

## Reading records

Patchbay has no ledger tail, pruning, or retention command. Use ordinary JSONL
tools:

```console
$ jq -r 'select(.status != 200) | [.timestamp, .context, .seat, .status] | @tsv' \
    ~/.local/state/patchbay/ledger/patchbay-v2-2026-08-20.jsonl

$ jq -s 'group_by(.seat) | map({seat: .[0].seat, requests: length})' \
    ~/.local/state/patchbay/ledger/patchbay-v2-*.jsonl

$ jq -r 'select(.drop_signature) | [.timestamp, .model, (.block_shape | join(","))] | @tsv' \
    ~/.local/state/patchbay/ledger/patchbay-v2-*.jsonl
```

The Go API is `(*ledger.Ledger).Scan(day)`. A missing day returns an empty slice.
Scanning stops at the first invalid line and returns the valid prefix with a
`*ledger.CorruptionError` naming the byte offset. A final line without a newline
is corruption.
