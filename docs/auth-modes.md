# Auth modes

Every route declares one `auth` mode.

| Mode | Upstream credential | Caller key | Accepted paths |
|---|---|---|---|
| `forward` | Caller credentials | No | Any |
| `inject` | Provider key file | Yes | `/v1/messages`, `/v1/messages/count_tokens` |
| `sigv4` | AWS profile credentials | Yes | `/v1/messages` |

The caller presents `X-Patchbay-Key`; patchbay compares it with the trimmed file
contents named by `PATCHBAY_CALLER_KEY_FILE`. A missing or wrong value returns
403. A missing, unreadable, or empty key file returns 500. The gate runs before
provider-key reads, model-map reads, AWS credential export, or upstream access.

## `forward`

A `forward` route preserves the caller's `Authorization` and `X-Api-Key` headers.
Validation requires `base_url` to be exactly `https://api.anthropic.com` so those
credentials cannot be sent to another host.

The built-in default is:

```json
{
  "auth": "forward",
  "base_url": "https://api.anthropic.com"
}
```

For `forward` and `inject`, patchbay appends the stripped inbound path and query,
preserves the method, removes hop-by-hop and proxy-owned headers, sets
`Accept-Encoding: identity`, never follows redirects, and relays the upstream
status, headers, and body.

An identity route preserves the exact request body bytes. Common route rewrites
cause JSON decoding and re-encoding; see [configuration.md](configuration.md).

## `inject`

An `inject` route removes the caller's `Authorization` and `X-Api-Key`, then sends:

```text
Authorization: Bearer <route key>
```

The key uses file indirection:

```json
"openrouter/kimi-k2": {
  "auth": "inject",
  "base_url": "https://openrouter.ai/api",
  "model": "moonshotai/kimi-k2",
  "api_key_env_file": "PATCHBAY_OPENROUTER_KEY_FILE"
}
```

`api_key_env_file` names an environment variable whose value is the key-file
path. The key file is read on every accepted request, so rotation does not need a
restart.

The request must first match one of the two accepted paths, then pass the caller
key, then provide a readable provider-key file. Other paths return 404. A key-file
failure returns 500 without contacting the upstream.

Validation rejects `claude` anywhere in an `inject` alias or configured model,
case-insensitively.

## `sigv4` -- Amazon Bedrock

A `sigv4` route translates the Anthropic request, maps the model, exports AWS
credentials, signs a Bedrock request, and translates streamed responses back to
Anthropic SSE.

```json
"default_route": {
  "auth": "sigv4",
  "aws_profile": "work-bedrock",
  "aws_region": "us-west-2",
  "model_map_env_file": "PATCHBAY_BEDROCK_MODEL_MAP_FILE"
}
```

The route must not set `base_url`. Its endpoint is:

```text
https://bedrock-runtime.<aws_region>.amazonaws.com
```

The outbound request is always `POST`. `stream: true` selects
`invoke-with-response-stream`; other requests use `invoke`.

### Model map

`model_map_env_file` names an environment variable containing the path to a JSON
object:

```json
{
  "claude-opus-4-5": "us.anthropic.claude-opus-4-5-20251101-v1:0",
  "claude-sonnet-4-5": "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
}
```

The lookup key is the route's configured `model`, or the request model when the
route does not rewrite it. The map is read on every request. A missing variable,
unreadable or invalid file, or incompatible value returns 500. A missing or empty
mapping returns 400. Introspection and `patchbay resolve` never print mapped
Bedrock identifiers.

### AWS credentials

patchbay runs:

```text
AWS_PROFILE=<aws_profile> aws configure export-credentials
```

`AccessKeyId` and `SecretAccessKey` are required. `SessionToken` and `Expiration`
are optional. Credential export has a 10-second timeout. Credentials are cached
until two minutes before expiration, or for 15 minutes when no expiration is
reported. Caches and refresh locks are per profile.

An export failure returns 502 and tells the operator to run:

```text
aws sso login --profile <profile>
```

CLI output and credential material are not returned to the caller or ledger.

### Request and retry

The signer uses service `bedrock`, the route's region, the payload hash, and the
fixed Bedrock request headers. Caller credentials and `X-Patchbay-Key` are
removed.

A Bedrock 403 triggers one credential refresh and retry. A second 403 becomes a
502 with the AWS login instruction. No other status is retried by this rule.

The path gate runs before caller-key validation, body parsing, map access, and AWS
credential export:

| Path | Result |
|---|---|
| `/v1/messages` | Translate and send to Bedrock. |
| `/v1/messages/count_tokens` | 501; Bedrock has no equivalent here. |
| Anything else | 404. |

After common route rewrites, the body must be a JSON object. Bedrock translation:

1. removes `model`;
2. removes `stream`;
3. sets `anthropic_version` to `bedrock-2023-05-31`.

Other fields are retained. The mapped Bedrock model ID goes in the invocation URL.

### Responses

Non-streamed responses are relayed without JSON translation. AWS event streams
are decoded and emitted as `text/event-stream` SSE. patchbay validates framing
and CRCs, decodes each embedded Anthropic event, flushes events as they arrive,
and emits an Anthropic-shaped error event for Bedrock exceptions or malformed
stream data.

Ledger capture and summaries use the translated SSE received by the client, not
the AWS framing.
