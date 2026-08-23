# patchbay

patchbay is a per-host gateway for the Anthropic Messages API. Claude Code sends
requests through `ANTHROPIC_BASE_URL`; patchbay selects a Context from the URL,
resolves the requested model to a global Seat, and forwards the request.

A Seat is a reusable upstream, authentication, rewrite, limits, and capture
configuration. A Context publishes exact model-selector-to-Seat-ID bindings and
names a default Seat. An optional marked-subagent policy can select separate
Seats when Claude Code sends a valid direct agent marker.

It is a single static CGO-free binary. It has three direct module dependencies:
a pure-Go SQLite driver, which backs the accounting database; `golang.org/x/sys`
for the descriptor-relative syscalls that harden the database path; and
`github.com/ebitengine/purego`, which is used only on Darwin, to call
libSystem's ACL functions through dynamic symbol lookup rather than CGO. `sigv4`
Seats also require the AWS CLI because patchbay runs
`aws configure export-credentials` for the configured profile.

## Resolution

```text
Claude Code                    patchbay                       upstream

ANTHROPIC_BASE_URL ----------> select Context
  http://127.0.0.1:4100        bare path: default_context
  .../ctx/work                  /ctx/work: work
                                      |
                               resolve model
                         +------------+------------+
                         |            |            |
                     public model   marked       default
                       binding     subagent        Seat
                         |          policy           |
                         +------------+--------------+
                                      |
                               select global Seat
                         +------------+------------+
                         |            |            |
                      forward       inject       sigv4
                         |            |            |
                       caller      provider key    AWS
                        auth          file       credentials
                         +------------+------------+
                                      |
                         relay response and append ledger v2
```

Selection precedence is exact and shared by the gateway and CLI:

1. exact `context.models` binding;
2. valid marked subagent plus exact `context.subagents.models` binding;
3. valid marked subagent plus `context.subagents.default_seat`;
4. refusal of a protected foreign rewrite target;
5. `context.default_seat`.

Steps 2 and 3 are both recorded as a `subagent` selection; step 3 names no model,
which is the only thing distinguishing the unnamed marked fallback from an exact
marked hit. Step 5 is recorded as a `default` selection, also naming no model.

Patchbay observes `x-claude-code-agent-id` only when exactly one value is present,
it is 1–256 bytes, and every byte is printable ASCII `0x21`–`0x7e`. The value is
used verbatim. `x-claude-code-parent-agent-id` uses the same validation but never
marks traffic by itself. Both headers continue upstream unchanged. They are
observations, not authentication, identity, authorization, or a security
boundary.

Patchbay has no auxiliary-call classifier, prompt matching, or semantic agent
inference. Unmarked traffic follows ordinary bindings. Any request carrying a
valid direct agent marker follows the configured marked-subagent policy.

## Quickstart

Build with Go 1.26, which downloads the pinned pure-Go SQLite module on the
first build:

```console
$ go build ./cmd/patchbay
```

Create a caller key. This local shared secret gates Seats that spend credentials
configured on the gateway:

```console
$ mkdir -p ~/.config/patchbay
$ head -c 32 /dev/urandom | base64 > ~/.config/patchbay/caller.key
$ chmod 600 ~/.config/patchbay/caller.key
```

Create `~/.config/patchbay/routes.json`:

```json
{
  "default_context": "personal",
  "seats": {
    "anthropic": {
      "upstream": "https://api.anthropic.com",
      "auth_mode": "forward"
    },
    "openrouter-glm-4-6": {
      "upstream": "https://openrouter.ai/api",
      "model": "z-ai/glm-4.6",
      "auth_mode": "inject",
      "api_key_env_file": "PATCHBAY_OPENROUTER_KEY_FILE",
      "billing": "metered",
      "strip_cache_control": true
    }
  },
  "contexts": {
    "personal": {
      "default_seat": "anthropic",
      "models": {
        "openrouter/glm-4.6": "openrouter-glm-4-6"
      }
    }
  }
}
```

`api_key_env_file` is an environment variable name. The variable contains the
path to the provider-key file; the registry contains neither the path nor the
secret. Do not put secrets in `extra_body` or other registry fields.

`billing` declares how an `inject` Seat's traffic is accounted for —
`subscription`, `metered`, or `self_hosted`. Every `inject` Seat must declare
it, and only an `inject` Seat may: nothing downstream can look at a request and
tell what an operator key draws on. A `forward` Seat's class is derived from the
credential the caller presented and a `sigv4` Seat is metered by the AWS account
that signed it, so a declaration on either is refused rather than ignored.

Write the provider key, validate the registry, and start patchbay:

```console
$ install -m600 /dev/null ~/.config/patchbay/openrouter.key
$ $EDITOR ~/.config/patchbay/openrouter.key

$ export PATCHBAY_CALLER_KEY_FILE=~/.config/patchbay/caller.key
$ export PATCHBAY_OPENROUTER_KEY_FILE=~/.config/patchbay/openrouter.key

$ ./patchbay check
OK /home/you/.config/patchbay/routes.json: 2 seats, 1 context (personal), 1 binding

$ ./patchbay serve
```

Initial registry loading is strict and happens before the listener binds. A
missing, unreadable, malformed, or invalid registry makes `serve` fail.

Configure Claude Code in user settings or a project's local settings:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100",
    "ANTHROPIC_CUSTOM_HEADERS": "X-Patchbay-Key: <contents of caller.key>"
  }
}
```

Keep the caller key out of shared project settings. If the project should share a
Context, commit only `ANTHROPIC_BASE_URL` and provide the header from user or
local settings.

Leave `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` unset when the default Seat
should use the Claude Code subscription login. Claude Code sends its stored OAuth
credential to patchbay, and patchbay forwards it only on `forward` Seats.

With this configuration, ordinary Claude model names use the Context default,
while `/model openrouter/glm-4.6` uses the OpenRouter Seat. Third-party gateways
may accept non-Claude selectors, but [Anthropic does not support](https://code.claude.com/docs/en/llm-gateway)
non-Claude models behind them.

See [examples/routes.json](examples/routes.json) for all three auth modes.

## Contexts and Seats

The URL path selects a Context:

| Base URL | Context |
|---|---|
| `http://127.0.0.1:4100` | configured `default_context` |
| `http://127.0.0.1:4100/ctx/work` | `work` |

The prefix is removed before forwarding. Both forms send `/v1/messages` to the
selected Seat.

A project can select a Context in `.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4100/ctx/work"
  }
}
```

Contexts fail closed. While the registry is valid, an unknown explicit Context
returns 404. A hot invalid or missing registry installs an invalid sentinel, and
all new proxy and model requests return 503 `api_error`; stale snapshots are not
used. Requests that already resolved finish with their immutable resolution. A
valid replacement restores service atomically on the next request.

`patchbay project [dir]` shows which Claude Code settings file wins, which Context
it selects, and that Context's public bindings.

## Caller key

`X-Patchbay-Key` must match the trimmed contents of
`$PATCHBAY_CALLER_KEY_FILE` for:

- `inject` and `sigv4` Seats, including marked-subagent selections;
- `GET` and `HEAD /_patchbay/registry`;
- `GET /_patchbay/usage/summary`;
- explicit-Context `GET /v1/models` requests.

The gate runs after Seat selection and before provider-key reads, model-map reads,
AWS credential export, or upstream access. Patchbay compares SHA-256 digests in
constant time and removes the header before contacting an upstream. A missing or
incorrect value returns 403. An unreadable or unconfigured caller-key file returns
500.

A `forward` Seat does not require the caller key because it uses only credentials
sent by the caller. The caller key is not general gateway authentication.

## CLI

```text
patchbay serve
patchbay contexts
patchbay seats
patchbay bindings --ctx <name>
patchbay resolve [--subagent] <context> <model>
patchbay project [dir]
patchbay check [--registry <path>]
patchbay usage snapshot <dest>
patchbay usage export <dest>
patchbay usage import <ledger-dir>
```

Inspection commands query the running gateway at `$PATCHBAY_LISTEN` and use
`$PATCHBAY_CALLER_KEY_FILE`. `resolve --subagent` supplies a valid synthetic
marker and uses the same resolution algorithm as runtime traffic. The `usage`
commands operate directly on `$PATCHBAY_USAGE_DB` and need neither a running
gateway nor a caller key. See [docs/cli.md](docs/cli.md).

## Models and introspection

Public model endpoints expose Context selectors, never Seat IDs or marked-subagent
policy. A forward default merges public bindings with Anthropic's model listing.
An inject default contributes its configured rewrite model. A SigV4 default
contributes non-empty model-map keys, never mapped Bedrock identifiers. Public
bindings shadow duplicate upstream IDs.

Unprefixed caller-key-gated `GET /_patchbay/registry` remains available while the
registry is invalid. It returns `registry_valid: false`, a sanitized
`validation_error`, `default_context: null`, and empty Seat and Context summaries.
Model and introspection endpoints do not write ledger records.

## Ledger

Every proxy-bound request is ledger-eligible, including registry, Context, body,
rewrite-target, caller-key, and selected-Seat failures. Records are written under
`$XDG_STATE_HOME/patchbay/ledger`, or
`~/.local/state/patchbay/ledger` when `XDG_STATE_HOME` is unset:

```text
patchbay-v2-YYYY-MM-DD.jsonl
```

`PATCHBAY_LEDGER_DIR` overrides the ledger directory. `PATCHBAY_USAGE_DB`
overrides the durable accounting database, which defaults to
`$XDG_STATE_HOME/patchbay/usage.sqlite` (or
`$HOME/.local/state/patchbay/usage.sqlite`). Unlike the ledger, Patchbay will
not start if the accounting database cannot be opened.

`PATCHBAY_RATE_CARDS` optionally names a JSON file containing immutable,
effective-dated per-million-token rates. Each entry has `model`, RFC3339
`effective_from`, `source`, and string-valued `rates_usd_per_million` fields:
`input`, `output`, `cache_read`, `cache_creation`, `cache_creation_5m`, and
`cache_creation_1h`. Patchbay parses rates exactly as decimal USD, and will not
start if the file cannot be read or parsed or conflicts with a card already
recorded in the usage database.

Selected records include Context, selection reason, Seat, effective upstream,
upstream model, requested model, auth mode, inbound credential class, configured
credential source, and any validated subagent observation. Pre-selection failures
omit selection-dependent fields. `capture_bodies: true` stores rewritten request
and captured response bodies.

Only v2 files are scanned; there is no legacy decoder. After relaying the client
response, Patchbay materializes the finalized accounting snapshot and ledger
record and submits that compact work to a fixed-capacity completion queue. A
bounded worker set durably finalizes accounting before appending the ledger copy;
a full queue applies backpressure after response relay rather than dropping
accounting. A capture or append failure drops the ledger record and logs a
warning without changing the response. Shutdown drains this queue only until its
bounded deadline, logs unfinished work loudly, and leaves any requests that did
not finalize as crash-equivalent pending rows. See [docs/ledger.md](docs/ledger.md).

## Accounting

### Usage backup and export

`patchbay usage snapshot <dest>` makes an atomic SQLite snapshot that is
consistent even while the daemon is writing. `patchbay usage export <dest>`
writes the same point-in-time data as deterministic JSONL: a version header,
then `rate_cards` rows and `accounting_requests` rows in ID order. Both commands
read the database without migrating or changing it, refuse an existing
destination, and create a 0600 destination file. Work in progress stays inside
an adjacent mode-0700 staging directory on the destination filesystem; Patchbay
validates and fsyncs the artifact, secures it to 0600, then publishes it with a
no-replace link and fsyncs the destination directory. An abrupt process or host
failure can leave that private staging directory behind. Each run holds an
exclusive lock on a lockfile inside its staging directory for the full operation.
A later tooling run considers only matching directories at least one hour old and
removes one only after acquiring its lock nonblocking; a held lock proves the
staging directory is live, while younger residue is retained until a later run.

The live `PATCHBAY_USAGE_DB` must remain on a local filesystem. Snapshot and
export destinations can be on any filesystem. Patchbay traverses the live
database path relative to open directory descriptors without following symlinks.
Every
preexisting ancestor must be a directory owned by root or Patchbay's effective
UID. A group- or world-writable traversal ancestor is accepted only when sticky-
directory semantics protect the next validated component; the immediate
database parent must never be group- or world-writable because SQLite opens WAL,
shared-memory, and rollback-journal sidecars there. Every existing `-wal`,
`-shm`, and `-journal` sidecar beside the database is itself validated before
SQLite receives the pathname — a regular file reached without following a
symbolic link, owned by root or the effective UID, not group- or world-writable,
and free of a nontrivial Darwin ACL — because a sidecar planted while the parent
was briefly exposed is read or replayed during the first connection. A rejected
sidecar is left exactly as it was found. A missing sidecar is safe: the accepted
immediate parent already excludes other local UIDs from creating one. A
foreign-owned ancestor is
refused even when its current mode is private because its owner can change that
mode after validation. A preexisting database leaf must likewise be owned by
root or the effective UID and must not be group- or world-writable; Patchbay
refuses rather than repairs write exposure because an already-open writable
descriptor survives chmod. That refusal covers the file's current state only —
an inode that passes every check here can still be held open for writing by a
UID that opened it during an earlier exposure, which no metadata check can
detect or revoke. See [Residual exposure](#residual-exposure-descriptors-retained-from-before-validation)
below for the disposition and remediation. Newly created ancestors and database leaves must
remain owned by the effective UID; ancestors require exact mode 0700, and the
database is secured to 0600 through its opened descriptor before SQLite receives
the pathname. On Darwin, every preexisting or newly created ancestor and
database leaf must also have no nontrivial extended ACL. ACL inspection failures
are fatal.

`patchbay usage snapshot` and `patchbay usage export` apply that same traversal
to their source in a read-only form: a missing component is refused rather than
created, and an accepted source is never chmodded or otherwise repaired. Both
then run a read-only `PRAGMA integrity_check` against the exact database that
connection opened, and refuse unless it reports the single result `ok`. Path
validation and schema validation cannot stand in for that. Schema validation reads
only the schema — `schema_migrations`, `sqlite_master`, the definitions it stores,
and the structure SQLite parses out of them — and never a table's rows, while
`VACUUM INTO` rebuilds secondary indexes from the table rows rather
than copying them, so a source whose index no longer agrees with its table
satisfies both while remaining a corrupt original — and a snapshot taken from it
would be internally consistent, certifying that corruption as a good backup.

Schema validation is a check of objects, not of names. The schema each version
produces is rebuilt from this build's own migrations, in a private in-memory
database that touches nothing on disk, and a database is held to everything the
migrations it records declared. An object is identified by its SQLite object type
together with its name, never by the name alone: SQLite keeps a namespace per type
and will hold a trigger and an index of one name side by side, so a name identifies
at most a namespace and a comparison keyed on it decides by whichever
`sqlite_master` row it read last. Each declared object is therefore looked up by
that full identity, on the table association its migration gave it, with
the columns they declared in the order and with the type, nullability, default and
primary-key role they declared, the keys and uniqueness they gave an index, and the
stored definition itself — so a table's constraints and a trigger's body are
compared, not merely its name. This is not redundant with the recorded version, and
no later step recovers it: a migration already recorded is skipped rather than
replayed, so an object dropped or redefined afterwards is never rebuilt. A database
whose `schema_migrations` rows are a valid prefix but whose `rate_cards` table is
gone reads as a supported older schema to a version check and reports `ok` to
`PRAGMA integrity_check`, because the pages that remain are coherent; so does one
whose rate-card immutability triggers keep their names over a body that has become
`SELECT 1`, and one whose unique partial index on finalized response ids has been
rebuilt without `UNIQUE`. The first fails only when something reads the table,
which on a restored backup means `patchbay serve` failing to start; the other two
never fail at all — they quietly stop enforcing what they were created to enforce,
leaving immutable rate cards editable and one response id billable twice. All are
refused here instead, before anything is staged or published.

Definitions are compared for meaning rather than for text: comments, whitespace and
keyword case are normalized away, while quoted strings and quoted identifiers are
compared byte for byte. Migrations are immutable, so a database written by an
earlier build stored the text of the same statements this build declares, and
formatting is the only drift the comparison has to absorb.

Additions are refused as well as omissions, because a schema is tampered with by
addition as readily as by subtraction and an addition is active rather than inert.
Every trigger and index attached to a table those migrations create —
`accounting_requests`, `rate_cards`, `schema_migrations` — must be one they
declared. A trigger nobody declared runs on every write to the table it is attached
to, so an `AFTER INSERT … DELETE FROM accounting_requests` on a database whose every
canonical definition is exact empties the ledger the first time Patchbay records a
request; an index nobody declared decides which writes that table accepts at all.
Nothing else catches either one: the extra object leaves every declared definition
comparing equal, reports `ok` to `PRAGMA integrity_check`, is copied forward intact
by `VACUUM INTO`, and survives the forward-migration proof, because migrations
create objects and never remove one they do not know about.

What exempts an addition from that sweep is the same full identity — object type
and name — standing on the table its migration attached it to. A name alone would
not: a trigger wearing the name of a declared index is a second object rather than
that index, so the index is still present and exact while the trigger runs beside
it, and exempting the trigger by name would wave through precisely the addition
this sweep exists to refuse. The same holds in reverse for an index taking a
declared trigger's name, and for either taking a declared name onto a different
managed table. A schema holding two `sqlite_master` rows under one identity is
refused outright rather than resolved to whichever row was read last.

Indexes on those tables are refused whether or not they are `UNIQUE`. A unique index
— whole-table, partial, or over an expression — rejects rows the migrations accept.
A nonunique one is not the safe residue it looks like, since a key over an expression
or a collation the writing build cannot evaluate fails every insert into the table it
covers; all of them are refused rather than a subset whose safety would have to be
re-argued index by index. The one exception is the index SQLite maintains for a
`UNIQUE` or `PRIMARY KEY` constraint, which `rate_cards` requires and no migration
names: those carry no stored `CREATE` text, and that absence is what identifies them.
A reserved `sqlite_` name prefix would not — `CREATE` refuses such a name, but a
principal who can write the file can insert the `sqlite_master` row directly and
SQLite honours the trigger it finds there, so exempting that prefix would leave one
namespace unexamined.

Away from those tables the contract stays a floor rather than an inventory: an
unrelated table or view, and the triggers and indexes attached to it, are neither
required nor refused, because nothing there runs unless something writes to that
table and Patchbay never does. A table whose name a pending migration wants is the
one contradiction left over, and it surfaces where it has to — when that migration
runs against the disposable clone and collides.

The destination is a separate trust boundary and is deliberately not held to
that local-UID policy. Snapshot and export destinations are routinely
all-squashed network exports — the deployed NAS target maps every file and
directory it serves to one fixed remote UID that Patchbay never runs as — so
local ownership evidence does not exist there, and demanding it would refuse
Patchbay's own output. Patchbay therefore neither traverses nor refuses
destination ancestors, and does not claim local-UID security over them: the
destination filesystem, its directory integrity, and its identity mapping are
trusted. Choose a destination whose server and export you already trust, because
a principal who controls that filesystem controls the artifact written to it.
Within that trust, integrity rests on the private mode-0700 staging directory,
its exclusive lock, and the no-replace link that publishes the finished file. The
snapshot Patchbay generates inside that staging directory is validated as its own
artifact rather than as an external source: it must be a regular file opened
without following a symbolic link, must parse as a supported usage database, must
pass its own `PRAGMA integrity_check`, and must present the same device and inode
before and after SQLite opens it.

A snapshot that records a supported older schema is published unchanged, but only
after Patchbay proves that schema still migrates forward. It byte-copies the
finished artifact into a disposable migration clone, created exclusively at mode
0600 beside the artifact in the same private staging directory, applies every
pending migration to the clone, and re-checks integrity and schema on the migrated
result. The clone is opened under the same destination trust the artifact gets,
not the strict source policy, because an all-squashed destination cannot supply
local ownership evidence for a file Patchbay just created there. The clone and any
`-wal`, `-shm`, or `-journal` file derived from it are removed whether the proof
succeeds or fails. The artifact itself is never migrated: it is what gets
published. The object contract above covers the versions a snapshot records; the
clone covers the ones it has yet to run, where a contradiction — an object already
sitting under a name a pending migration needs — surfaces only when that migration
executes. Either way it fails here rather than being restored later into a
`patchbay serve` that cannot start.

`patchbay usage export` writes the current schema's columns whatever schema its
source records, so an older source has to be brought forward before it can be read
that way. It is never brought forward in place: the source is a live accounting
database and a read command leaves it byte for byte as it found it. Export copies
it instead — through SQLite, so the copy agrees with a live WAL writer rather than
being a torn read of the file — into a disposable migration clone inside the same
private staging directory the output is built in, secured to mode 0600 through the
clone's own descriptor, and it migrates and reads that copy. The clone is held to
the same destination trust the snapshot artifact gets, and to the same checks once
the migrations have run: `PRAGMA integrity_check`, and the recorded schema objects
at the current version. The clone and any `-wal`, `-shm`, or `-journal` file
derived from it are removed whether the export succeeds, fails, or is canceled, so
the finished export is the only thing that reaches the destination. A source
already at the current schema is read directly, with no copy and no migration.

These owner, mode, sticky-directory, and Darwin ACL checks exclude another
local UID from ownership or replacement rights over validated components before
SQLite opens the path. The validation descriptors are then closed before the
SQLite driver opens it.
Code running as Patchbay's effective UID or root can still replace validated
components and remains inside the live-path trust boundary. Patchbay rechecks
the opened database's device and inode after SQLite establishes its first
connection to detect replacement during that initial open; it does not claim to
defend later live-path mutation by those trusted principals.

#### Residual exposure: descriptors retained from before validation

Every check above is a statement about the pathname's *current* state. Together
they establish present ownership, mode, and ACL, and they prevent a foreign UID
from opening or replacing a validated component from validation onward. What they
cannot do is detect or revoke a foreign writable descriptor retained from an
earlier exposure. They establish nothing about how that file was reachable in the
past, and that limit is structural rather than an implementation gap.

The counterexample is concrete. Suppose the database or one of its sidecars was
briefly mode 0666, or briefly owned by another UID, and during that window a
process running as a different UID opened it `O_RDWR`. Suppose the owner and mode
are then tightened to values this policy accepts. The retained descriptor keeps
write access to that same inode: an open file description is capability-like, and
neither the later `chmod` nor the later `chown` revokes it. That process can write
the file before or during `Ping`, including into a sidecar SQLite is about to
replay. The device and inode recheck cannot see it, because nothing about the
pathname changed — it is the same inode, which is exactly the problem. No portable
combination of `stat`, `openat`, and permission inspection can enumerate the open
descriptors held against an inode, prove none exist, or revoke one.

Treat any live database or sidecar that was ever exposed — ever world-writable,
ever group-writable to an untrusted group, or ever owned by a foreign UID — as
compromised, even if it now passes every check and its contents look intact.
Passing validation afterwards says nothing about whether a descriptor survived
from the exposure window; it is only evidence about the pathname today.

The remediation is a fresh inode, because that is the one operation a retained
descriptor cannot follow. Do not reuse the suspect inode by truncating it,
chmodding it, chowning it, or writing over it in place; an ordinary `cp` onto the
resolved `$PATCHBAY_USAGE_DB` path does exactly that, because it opens the
existing file and truncates it. The pathname then holds restored data in the same
inode the foreign process still has open for writing, so that process can write
the restored database. The suspect `-wal`, `-shm`, and `-journal` sidecars come
from the same exposure window and must be discarded rather than copied forward or
left beside the new file.

With Patchbay stopped, restore a trusted snapshot onto a new inode in this order.
Create a fresh temporary file with `O_CREAT|O_EXCL`, or a securely-created
`mktemp` file, in the same secure local parent directory that holds the resolved
`$PATCHBAY_USAGE_DB` path, so the replacement is built where this policy already
excludes other local UIDs. Exclusive creation is what makes it a new inode: a
plain create can reopen a name that already exists and hand the replacement back
to whatever descriptor is already attached to it. Copy or restore the trusted
snapshot into that new file, set it to mode 0600, fsync it, and close it.

Validate that fresh candidate before publishing it, while the suspect database
and its sidecars are all still exactly where they are. Point
`PATCHBAY_USAGE_DB` at the fresh candidate path and run
`patchbay usage snapshot <throwaway>` against a throwaway `.db` destination that
does not already exist, in a trusted directory of your choosing. Usage tooling
reads `$PATCHBAY_USAGE_DB` directly and needs neither a running gateway nor a
caller key, so this runs mid-procedure with Patchbay still stopped. The
destination has to be a path that does not exist yet because the tooling refuses
to publish over an existing file, which also keeps a stale artifact from an
earlier attempt from being mistaken for this attempt's evidence.

A zero exit status is the evidence to publish on. The validating read opens the
candidate through the same strict path traversal the live database gets, and in
read-only mode, so success establishes that the candidate is the regular file at
the path named, that it is readable SQLite rather than a truncated or corrupt
copy, and that it is a Patchbay usage database this build supports — not an
unrecognized file, not an unknown future schema, and not a database missing a
table, column, index, or trigger the version it records promised or holding one
that keeps its name over a weakened definition. A read-only
`PRAGMA integrity_check` runs against that exact opened candidate and must report
the single result `ok`, so every page reads back coherently and every secondary
index still agrees with the table it indexes, rather than only the header a wrong
file could still carry. It accepts an older schema that `patchbay serve` would
migrate forward, so this gate does not narrow which snapshots are restorable —
and it proves that forward migration rather than inferring it from the recorded
version. Every pending migration is applied to a disposable clone of the
command's own output, and integrity and schema are rechecked on the migrated
clone, so a candidate that claims a supported older version but contradicts it on
disk fails the gate instead of being restored into a `patchbay serve` that cannot
start. The read does not modify the candidate and leaves no sidecar beside it;
the clone is a copy, it is what gets migrated, and it is removed either way.
Remove the throwaway output once the command exits zero; it is produced to prove
the read succeeded, not to be kept.

Any validation failure aborts the restore. Nothing has been removed or renamed
yet, which is exactly why aborting is still free: leave the suspect database and
all of its `-wal`, `-shm`, and `-journal` sidecars untouched, remove only the
fresh candidate and any partial validation output, and start again from a
snapshot that validates. Publishing first and validating afterwards is a
different procedure with no recovery in it — once the sidecars are gone and the
rename has landed, a truncated, corrupt, wrong, or future-schema candidate has
already replaced the database, and the sidecars that would have been needed to
recover it are gone too.

Only after validation succeeds, publish. Remove every suspect `-wal`, `-shm`,
and `-journal` sidecar at the resolved path — a surviving sidecar is read or
replayed into the restored database on the first connection. Leave the suspect
database itself in place and rename the fresh file directly onto the resolved
path. That rename replaces the suspect inode with the
fresh one in a single atomic step, which is the fresh-inode replacement this
remediation depends on, and it leaves no window in which the resolved path is
missing. Then fsync the parent directory so the rename survives a crash. Start
Patchbay only after the replacement is in place; `patchbay serve` opens the
restored database and automatically migrates an older-schema snapshot forward
before serving.

### Legacy ledger import

`patchbay usage import <ledger-dir>` imports recognized `patchbay-YYYY-MM-DD.jsonl`
and `patchbay-v2-YYYY-MM-DD.jsonl` files in filename order. Each imported row
keeps its ledger basename and one-based line number as `import_source` and
`import_line`; live accounting rows leave both columns null. That provenance
makes a rerun safe and lets a resumed import skip only records already stored.
Idempotence assumes ledger files are immutable: changing an imported line, or
importing a different file with the same basename, retains the originally
imported accounting row for that basename and line number. Patchbay reads,
decodes, validates, and derives each chunk before taking the write permit; a
chunk is bounded to 500 lines and 32 MiB of source data, then commits atomically
in a short transaction. Live request writes can complete between chunks.

Before the first file, Patchbay fixes one overlap boundary at the earliest live
SQLite `started_at`. If the live table is empty, the boundary is the instant the
boundary read begins: older ledger records import, while records starting at or
after that instant are skipped. The command prints imported, boundary-skipped,
and already-present counts for each file.

The import can run while the daemon is serving, but a fresh empty database has a
milliseconds-scale residual race: a request may have started before the boundary
while its `InsertPending` has not committed yet, then append a ledger line later.
For a strictly overlap-safe fresh-database migration, run the import before the
daemon starts serving.

Legacy ledgers do not contain complete pricing or normalization evidence. Imported
subscription rows are known zero-marginal cost; imported metered rows deliberately
remain unknown cost and are never rate-card priced.

### Usage summary

`GET /_patchbay/usage/summary?since=<RFC3339>&until=<RFC3339>[&context=<name>]`
returns the half-open `[since, until)` aggregate from the durable accounting
database. `since` and `until` are required; `context` limits the aggregate to
one context when supplied. The caller key is required.

The response has a stable wrapper with canonical UTC window bounds:

```json
{
  "since": "2026-08-20T12:00:00Z",
  "until": "2026-08-20T13:00:00Z",
  "context": "work",
  "summary": {
    "requests": {"messages": 12, "count_tokens": 3, "other": 1},
    "messages": {
      "finalized": 10,
      "pending": 2,
      "known_cost_nano_usd": 123456789,
      "unknown_cost_rows": 1,
      "input_tokens": 1000,
      "output_tokens": 200,
      "cache_read_tokens": 50,
      "cache_creation_total": 25,
      "by_billing_class": {
        "metered": {"requests": 8, "known_cost_nano_usd": 123456789, "unknown_cost_rows": 1}
      },
      "by_cost_basis": {
        "provider_reported": {"requests": 7, "known_cost_nano_usd": 123456789, "unknown_cost_rows": 0}
      }
    }
  }
}
```

All money is integer nano-USD. `unknown_cost_rows` counts finalized message rows
whose cost cannot be established; it is never estimated. Null billing classes
and cost bases are grouped as `"unknown"`. Cache creation totals use 5-minute
and 1-hour values when either is present, otherwise the base cache-creation
value, so one request is never counted twice.

## Security

Keep the listener on loopback. Claude Code sends its subscription credential to
patchbay over HTTP, and the caller key does not gate `forward` Seats.

Provider keys and Bedrock model maps use file indirection and are read per request.
AWS credentials are exported through the AWS CLI and cached until near their
reported expiration, or for 15 minutes when no expiration is reported.

Registry validation is structural and referential only. It does not read provider
keys or model maps, export AWS credentials, validate AWS sessions, probe upstreams,
or otherwise spend configured credentials.

Patchbay also:

- never follows redirects;
- restricts `forward` Seats to `https://api.anthropic.com`;
- removes caller credentials before `inject` and `sigv4` requests;
- accepts plain HTTP only for loopback or an `inject` Seat with `insecure: true`;
- removes hop-by-hop headers in both directions;
- returns Anthropic-shaped gateway errors without credential values.

The container image does not include the AWS CLI and cannot serve `sigv4` Seats
without modification.

## Documentation

- [Configuration](docs/configuration.md) — strict registry, Seats, bindings,
  marked subagents, reload, endpoints, and limits
- [Auth modes](docs/auth-modes.md) — `forward`, `inject`, and `sigv4`
- [CLI](docs/cli.md) — commands, output, and introspection
- [Ledger](docs/ledger.md) — v2 schema and reading records
- [Nix](docs/nix.md) — flake outputs and Home Manager module
- [Docker](docs/docker.md) — image and Compose setup
- [ChatGPT subscription](docs/chatgpt-subscription.md) — optional CLIProxyAPI
  integration and account risk

## Building and testing

```console
$ go build ./cmd/patchbay
$ go test ./...
$ just check
$ nix build .#patchbay
```

`just check` runs lint, race-enabled tests, the coverage gate, and the Nix pin
check.

## License

MIT — see [LICENSE](LICENSE).
