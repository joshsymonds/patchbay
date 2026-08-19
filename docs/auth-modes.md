# Auth modes

Every route declares exactly one `auth` mode, and that mode decides what pays
for the request. The three are `forward`, `inject` and `sigv4`.

| | pays with | caller key | path gate | body |
|---|---|---|---|---|
| `forward` | the caller's own credentials | not required | none | byte-faithful unless the route rewrites |
| `inject` | a key patchbay reads from a file | **required** | `/v1/messages`, `/v1/messages/count_tokens` | byte-faithful unless the route rewrites |
| `sigv4` | AWS credentials exported from a profile | **required** | `/v1/messages` only | always translated for Bedrock |

## `forward`

The default, and the built-in fallback. The caller's `Authorization` and
`X-Api-Key` headers are left exactly as they arrived, and the request goes to
`https://api.anthropic.com` — the only upstream validation will accept for this
mode, because those headers may carry a subscription OAuth token.

Byte fidelity is the invariant. On a route that rewrites nothing, the body
forwarded upstream is the caller's original bytes, never decoded and re-encoded,
and the response is streamed back flush by flush exactly as it arrived. The
ledger tee is passive: any failure to capture or record drops the capture, never
the stream.

What patchbay does touch, on every relayed request in any mode:

- hop-by-hop headers (RFC 9110) and everything the message's own `Connection`
  header names are dropped;
- `Host`, `Content-Length` and `Accept-Encoding` are dropped, and
  `Accept-Encoding: identity` is set — byte fidelity is only meaningful over
  bytes patchbay can hand on unchanged;
- `X-Patchbay-Key` is stripped, since it is patchbay's own header and no
  upstream's business;
- redirects are never followed. The upstream's 3xx is relayed as the answer.

The inbound path and raw query are appended to the route's `base_url` verbatim,
so a `forward` route relays every endpoint, not just Messages — which is why an
unconfigured patchbay is a working Anthropic passthrough.

The built-in default route (`forward` to `https://api.anthropic.com`, no
rewrite, no capture) is a fixed value in the code. It is never read from the
registry, so no registry failure can remove it, and its path never reaches a
gate: a request that resolves to it is never refused for want of a caller key.

## `inject`

The route drops the caller's credentials — both `Authorization` and `X-Api-Key`
are deleted, so an OAuth token never travels alongside an injected key — and
sets `Authorization: Bearer <key>` from the file the route names.

```json
"openrouter/kimi-k2": {
  "auth": "inject",
  "base_url": "https://openrouter.ai/api",
  "model": "moonshotai/kimi-k2",
  "api_key_env_file": "PATCHBAY_OPENROUTER_KEY_FILE",
  "strip_cache_control": true
}
```

`api_key_env_file` is the *name* of an environment variable. Its *value* is a
path. The file at that path holds the key. The registry never holds a secret or
even a path, so it can live in `/nix/store` or in git. The file is read per
request, so rotating the key on disk takes effect on the next turn with nothing
to restart, and nothing keeps the key in memory afterwards.

Two gates run before the key is read, in this order:

1. **Path.** An inject route relays only `/v1/messages` and
   `/v1/messages/count_tokens`. Anything else is 404 — the route exists to
   answer Messages turns, and a stray path must not spend the key on some other
   endpoint.
2. **Caller key.** The request must present `X-Patchbay-Key` matching the
   contents of `$PATCHBAY_CALLER_KEY_FILE`. Missing or mismatched is 403 and
   the error body never echoes either value; a key file the gateway cannot read
   is 500.

Most foreign upstreams do not understand Anthropic's prompt-cache markers, so
`strip_cache_control: true` is usually right here. `extra_body`, `model` and
`max_output_tokens` all apply as described in
[configuration.md](configuration.md).

The alias and the rewrite target may not contain `claude`, case-insensitively.
A foreign key paying for something that reads as a Claude model is a billing lie
in the ledger, and validation refuses to write it.

## `sigv4` — Amazon Bedrock

The one mode that does not relay. A `sigv4` route translates the request into
Bedrock's own shape, signs it with AWS SigV4, and translates the answer back.

```json
"default_route": {
  "auth": "sigv4",
  "aws_profile": "work-bedrock",
  "aws_region": "us-west-2",
  "model_map_env_file": "PATCHBAY_BEDROCK_MODEL_MAP_FILE"
}
```

### Endpoint

There is no `base_url`; validation refuses one. The endpoint is derived from the
region, as `https://bedrock-runtime.<aws_region>.amazonaws.com`, so the URL and
the signature can never disagree about where the request is going. The final
target is:

```
https://bedrock-runtime.us-west-2.amazonaws.com/model/<escaped bedrock id>/invoke
https://bedrock-runtime.us-west-2.amazonaws.com/model/<escaped bedrock id>/invoke-with-response-stream
```

whichever suffix matches the request's own `"stream"` field. The Bedrock id is
percent-escaped per RFC 3986, colons and all.

### Model map

The Bedrock model id is never in the registry. `model_map_env_file` names an
environment variable whose value is the path of a JSON object mapping the model
the request asks for to the Bedrock id to invoke:

```json
{
  "claude-opus-4-5": "us.anthropic.claude-opus-4-5-20251101-v1:0",
  "claude-sonnet-4-5": "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
}
```

The lookup key is the route's `model` when it sets one, otherwise the model the
caller named. The map is read per request, so a rebuild takes effect
immediately. A model with no entry — or an entry that is the empty string — is
refused with **400** `invalid_request_error`, naming the model but never the
mapping. A request that names no model at all is refused the same way.

The indirection exists so no account-identifying inference-profile id or ARN
ends up in a registry file you might share or check in. `patchbay resolve` says
only whether a model is mapped, never to what.

### Credentials

patchbay never reads `~/.aws` itself. It runs:

```
AWS_PROFILE=<aws_profile> aws configure export-credentials
```

inheriting the rest of the gateway's environment, and parses `AccessKeyId`,
`SecretAccessKey`, `SessionToken` and `Expiration` out of the result. The AWS
CLI already knows how to refresh an SSO session; duplicating that is how a
gateway ends up with its own subtly wrong idea of what a profile means. Every
argument is a constant and the profile is passed through the environment, so
nothing patchbay reads can become part of a command line.

- **Timeout**: one export is bounded at 10 s.
- **Caching**: per profile, until 2 minutes before the credential's stated
  expiry, or 15 minutes when it states none (which is how a rotated long-lived
  key pair takes effect without a restart).
- **Single flight**: the per-profile lock is held across the exec, so a hundred
  concurrent turns that need a refresh cost exactly one `aws` process. One
  profile's refresh never blocks another's.
- **Sanitized failures**: an export failure is reported as one short sentence
  naming only the profile. The CLI's own output is dropped — it can carry
  credential material, and everything patchbay reports reaches both a ledger
  record and a caller-visible error body.

An export failure answers **502** with the actionable advice:

```
patchbay: aws credentials for profile "work-bedrock" are unavailable; run: aws sso login --profile work-bedrock
```

Signing covers the host, the method, the canonically-escaped path and query, the
payload hash, and a fixed set of headers — `accept`, `content-type`,
`x-amz-date` and `x-amz-security-token` — when present. Signing a fixed set is
deliberate: a header patchbay does not control must never be able to change the
signature. Nothing from the AWS SDK is involved; `internal/awssig` is
stdlib-only, on the grounds that the SDK would be an order of magnitude larger
than patchbay to sign one request shape against one service.

### The 403 retry

AWS refuses a signature made with a credential that has since expired, which is
the ordinary end of an SSO session rather than something worth surfacing. So a
`sigv4` route gets exactly one retry, and only on 403: the cached credential is
invalidated, a fresh one is exported, and the request is rebuilt and re-signed.
A second 403 is patchbay's failure to report, not the caller's — the caller has
no AWS credential of its own to fix — so it becomes **502**:

```
patchbay: bedrock refused the request signed with profile "work-bedrock" twice; run: aws sso login --profile work-bedrock
```

### Path gate

| Path | Answer |
|---|---|
| `/v1/messages` | proxied to Bedrock |
| `/v1/messages/count_tokens` | **501** — Bedrock has no equivalent, and an honest "not implemented" is worth more than a 404 that reads like a broken gateway. Claude Code asks for this endpoint routinely; the 501 is expected and harmless. |
| anything else | **404** `not_found_error` |

The caller-key gate applies here exactly as it does to `inject`, and it runs
before any credential is exported or any signature is made.

### Body translation, exactly

The request document is walked as raw JSON values rather than a typed struct, so
a field this build has never heard of still reaches the model. The translation
is three operations and no others:

1. delete `model` — Bedrock carries it in the URL;
2. delete `stream` — Bedrock carries it in the URL suffix;
3. set `anthropic_version` to `bedrock-2023-05-31`.

Everything else the caller wrote — `system`, `messages`, `tools`, `thinking`,
`max_tokens`, anything — rides along byte-identically. Note that a `sigv4` route
is never on the identity fast path: the document is always decoded and
re-encoded, so key order may differ from what the caller sent.

Outbound headers: the caller's headers minus the ones a proxy owns, with
`Authorization` and `X-Api-Key` deleted, `Content-Type` and `Accept` set to
`application/json`, and the SigV4 headers added by the signer.

### Response translation

A non-streamed answer is JSON already in Anthropic's shape and is relayed
unchanged.

A streamed answer arrives as `application/vnd.amazon.eventstream` — AWS's binary
framing — and is decoded and re-emitted as the `text/event-stream` SSE that an
Anthropic client speaks, one event at a time, flushed as it arrives:

- each frame's payload is a one-field object whose `bytes` are the Anthropic
  event JSON;
- the SSE event name is the `type` the event JSON names itself, falling back to
  the frame's `:event-type` header;
- a frame carrying `:exception-type` becomes an `event: error` with an
  Anthropic-shaped error body, and ends the stream;
- a frame carrying neither an event nor an exception is skipped;
- prelude and message CRCs are verified, and a frame that does not decode ends
  the turn — the events already written are real, everything after them is not.

The caller's `Content-Type` is rewritten to `text/event-stream`, and the ledger
summarizes the SSE the caller actually received rather than the framing it
arrived in.
