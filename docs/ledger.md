# Ledger

Requests that enter the proxy pipeline can produce one JSON object per line:

```text
$PATCHBAY_LEDGER_DIR/patchbay-YYYY-MM-DD.jsonl
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

Model and introspection endpoints do not write records. Requests rejected after
entering the proxy pipeline can still produce records.

## Schema

```json
{"timestamp":"2026-08-19T00:22:56.18060013-07:00","host":"workstation","context":"personal","credential_class":"none","alias":"stub/kimi","upstream":"http://127.0.0.1:4300","model":"moonshotai/kimi-k2","request_model":"stub/kimi","auth_mode":"inject","status":200,"input_tokens":12,"output_tokens":5,"duration_ms":0,"upstream_ms":0,"block_shape":["text"],"drop_signature":false,"stop_reason":"end_turn"}
```

| Field | Meaning |
|---|---|
| `timestamp` | Request start time. Its UTC date selects the file. |
| `host` | Gateway hostname. |
| `context` | Selected context; bare requests use `personal`. |
| `credential_class` | Inbound credential shape: `oauth`, `api_key`, or `none`. |
| `alias` | Selected alias, empty for a default route. |
| `upstream` | Effective upstream attached to the exchange. It does not prove a request reached that upstream. |
| `model` | Route rewrite target, requested model, or `(none)`. SigV4 records the map key, not the Bedrock ID. |
| `request_model` | Model before rewriting, or `(none)`. |
| `auth_mode` | `forward`, `inject`, or `sigv4`. |
| `status` | Status returned to the client. |
| `input_tokens`, `output_tokens` | Counts parsed from a successful response, otherwise zero. |
| `duration_ms` | Whole milliseconds from request arrival through response copy or gateway error. |
| `upstream_ms` | Whole milliseconds until upstream response headers, or zero. |
| `block_shape` | Ordered content-block types; `null` when no response capture was summarized. |
| `drop_signature` | True when adjacent reasoning blocks appear in `block_shape`. |
| `stop_reason` | Parsed stop reason, omitted when empty. |
| `error_body` | Gateway error or non-2xx upstream response, omitted when empty. |
| `request_body`, `response_body` | Bodies stored only when `capture_bodies` is enabled and capture succeeds. |

For SigV4, `request_body` is captured after common route rewrites but before
Bedrock removes `model` and `stream` and adds `anthropic_version`. Bedrock response
streams are captured after translation to Anthropic SSE.

On a non-2xx upstream response, retained bytes become `error_body`. With body
capture enabled, the same bytes also remain in `response_body`.

## Credential handling

The schema has no request-header or response-header fields. Authorization,
API-key, caller-key, SigV4, and AWS session-token headers therefore have no place
in a record.

`credential_class` records only header shape:

1. Bearer `Authorization` -- `oauth`;
2. otherwise a non-empty `X-Api-Key` -- `api_key`;
3. otherwise -- `none`.

Captured bodies may contain sensitive application data. Enable `capture_bodies`
only when the ledger's storage and retention are suitable.

## Validation and compatibility

Every appended or scanned record is validated. Required non-blank fields are
`host`, `upstream`, `model`, and `request_model`. Auth mode, credential class, and
context must use their accepted values; timestamp must be nonzero; status, token
counts, and durations must be non-negative.

Older records may omit `context` and `credential_class`; empty values remain
accepted for that compatibility. The current proxy populates both fields.

`(none)` is the proxy's value when no usable request model was available.

## Capture limits

`error_body` is truncated to 4096 bytes on a UTF-8 boundary.

The response summary capture is limited to 64 MiB. If a response exceeds that
limit, the complete capture is discarded rather than stored as a prefix. The
client still receives the response, and the record is attempted without summary
or response body. Request bodies have their own 64 MiB proxy limit.

A client-write failure, upstream read error, or Bedrock stream decoding error also
discards the response capture. A clean EOF is not checked for an Anthropic
`message_stop` event; a clean prefix may still be summarized.

Only retained 2xx responses are summarized. JSON responses provide block types,
usage, and stop reason. SSE responses provide those values from
`message_start`, `content_block_start`, and `message_delta` events.

## Reading records

Patchbay has no ledger tail, pruning, or retention command. Use ordinary JSONL
tools:

```console
$ jq -r 'select(.status != 200) | [.timestamp, .context, .alias, .status] | @tsv' \
    ~/.local/state/patchbay/ledger/patchbay-2026-08-19.jsonl

$ jq -s 'group_by(.context) | map({context: .[0].context, requests: length})' \
    ~/.local/state/patchbay/ledger/*.jsonl

$ jq -r 'select(.drop_signature) | [.timestamp, .model, (.block_shape | join(","))] | @tsv' \
    ~/.local/state/patchbay/ledger/*.jsonl
```

The Go API is `(*ledger.Ledger).Scan(day)`. A missing day returns an empty slice.
Scanning stops at the first invalid line and returns the valid prefix with a
`*ledger.CorruptionError` naming the byte offset. A final line without a newline
is treated as corruption.
