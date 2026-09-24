# Haitatsu

Haitatsu is a simple email server written in Go. It receives and stores mail, exposes IMAP and SMTP submission for clients, and provides a REST API for trusted backend services to manage mailboxes and messages.

## Features

- IMAP4rev1 with stable per-folder UIDs, IDLE push, server-side SEARCH, MOVE, UIDPLUS, ESEARCH, SPECIAL-USE, LIST-STATUS, and label folders under `Labels/`
- SMTP inbound with SPF, DKIM, DMARC (relaxed and strict alignment), DNSBL, sender allow/block lists, per-IP connection and rate limits
- SMTP submission with PLAIN and LOGIN auth, DKIM signing, Bcc stripping, per-mailbox outbound limits, send-as for routed aliases
- Relay delivery with exponential backoff over roughly two days, permanent failure notifications
- REST API with cursor pagination, constant-time service token auth
- TLS from automatic ACME certificates, or read straight out of a certmagic-layout S3 bucket that something else (such as Caddy) already keeps current, for any number of hostnames
- App passwords for protocol access, with database-backed login throttling shared across nodes
- Quota accounting that tracks deletes and expunges, with a recompute endpoint
- PostgreSQL, SQLite, or remote libSQL for metadata, S3-compatible storage for message blobs, versioned migrations
- Pkl configuration with hot reload of spam, relay, webhook, notification, API token, and limits settings

Mailbox users do not call the REST API directly. Integrate from your own backend using a configured service token. End users authenticate to IMAP and SMTP with app passwords.

## Requirements

- Go 1.26 or later
- [Pkl](https://pkl-lang.org/) (for local runs outside Docker)
- PostgreSQL, a writable local directory for SQLite, or a libSQL server
- S3-compatible object storage (MinIO works for development)

## Configuration

Copy the example config and edit it for your environment:

```sh
cp haitatsu.example.pkl haitatsu.pkl
```

The server reads config from `/etc/haitatsu/haitatsu.pkl` by default and refuses to start if nothing is there. Override with `-config path/to/haitatsu.pkl`. The Docker image ships no config, so mount one at that path.

Any field can read from the environment with `read("env:NAME")`, and `read?("env:NAME") ?? ""` makes it optional. The example config does this for every secret, including the database DSN and libSQL auth token, and sets `instance_name` from `HOSTNAME`. See `deploy/README.md` for the list.

## Database backends

PostgreSQL remains the default. The old `postgres { dsn = "..." }` block is still accepted, but new configurations should use `database`:

```pkl
database {
  driver = "postgres"
  dsn = "postgres://haitatsu:password@localhost:5432/haitatsu"
  auth_token = ""
  namespace = ""
}
```

For a local SQLite file:

```pkl
database {
  driver = "sqlite"
  dsn = "file:/var/lib/haitatsu/haitatsu.db"
  auth_token = ""
  namespace = ""
}
```

Haitatsu enables foreign keys, WAL mode, and a five-second busy timeout for local SQLite connections. Keep the database on a local persistent volume, not a network filesystem.

For Turso Cloud:

```pkl
database {
  driver = "libsql"
  dsn = "libsql://database-name-organization.turso.io"
  auth_token = read("env:HAITATSU_DATABASE_AUTH_TOKEN")
  namespace = ""
}
```

Self-hosted sqld accepts `http://`, `https://`, `ws://`, and `wss://` URLs. An unauthenticated local server can use:

```pkl
database {
  driver = "libsql"
  dsn = "http://sqld:8080"
  auth_token = ""
  namespace = "haitatsu"
}
```

`namespace` sends the `x-namespace` header expected by a self-hosted `sqld`
server. Leave it empty for Turso Cloud and single-database servers.

SQLite and libSQL use FTS5 for the REST message search endpoint. PostgreSQL keeps its GIN-backed full-text index.

## Local development

Start Postgres and Garage, then build and run Haitatsu:

```sh
docker compose up -d postgres garage
task build
./haitatsu -config haitatsu.pkl
```

For a host-run Haitatsu process, point the local config at Garage on
`127.0.0.1:9000` with region `us-east-1`, bucket `haitatsu`, and the development
access key and secret declared in `compose.yaml`.

Or run the full stack (Postgres, Garage, and Haitatsu in Docker):

```sh
task compose:up
```

The compose stack publishes:

| Service   | Port  |
|-----------|-------|
| HTTP API  | 8080  |
| SMTP      | 2525  |
| IMAP      | 1143  |
| Submission (STARTTLS) | 1587 |
| Submission (TLS)      | 1465 |

Health checks: `GET /health`, `GET /ready`. Metrics: `GET /metrics`.

## Configuration reference

Listener addresses, database settings, S3, TLS, and worker enablement require a restart. Everything else reloads on `SIGHUP` or `POST /api/v1/admin/reload`.

| Block | Keys |
|-------|------|
| `database` | `driver`, `dsn`, `auth_token`, `namespace` |
| `limits` | `max_message_size_bytes`, `max_inbound_recipients`, `max_submission_recipients`, `max_connections_per_ip`, `inbound_messages_per_minute_per_ip`, `default_outbound_per_hour`, `default_outbound_per_day`, `default_outbound_recipients_per_message` |
| `relay` | `addr`, `username`, `password`, `from_host`, `max_attempts`, `max_retry_minutes` |
| `webhooks` | `default_timeout_seconds`, `secret`, `endpoints`, `max_attempts` |
| `spam` | `junk_threshold`, `reject_threshold`, `dnsbl_zones`, `dnsbl_score`, `require_helo`, `typesafe` |
| `spam.typesafe` | `mode`, `api_key`, `model`, `timeout_ms`, `max_text_bytes`, `max_in_flight`, `max_requests_per_minute`, `spam_threshold`, `phishing_threshold` |
| `imap` | `addr`, `max_connections_per_ip` |
| `submission` | `starttls_addr`, `tls_addr`, `allow_insecure_auth` |

TLS has four modes. `manual` loads `cert_file` and `key_file`. `acme` obtains a certificate for `public_hostname` itself using HTTP-01 or TLS-ALPN-01 on the listener host, cached under `acme_cache_path` (default `/var/lib/haitatsu/certmagic`). `storage` issues nothing and instead reads certificates from the S3 bucket in `tls.storage`, in the layout certmagic writes (`<prefix>/certificates/<issuer>/<host>/<host>.crt` and `.key`), for `public_hostname` plus every name in `storage.hostnames`, re-reading them every `refresh_interval_minutes` (and every 30 seconds while any are still missing, so it can start before the issuer has produced them). Use `storage` when a reverse proxy such as Caddy already owns ports 80 and 443 and keeps a shared certificate store, so replicas serve the same certificate without any of them talking to the CA; the bucket credentials only need read access. `off` disables TLS and allows plaintext IMAP authentication, which is only for local development. Submission never accepts AUTH over an unencrypted connection unless `submission.allow_insecure_auth` is explicitly set to `true`.

Per-mailbox outbound limits override the defaults through the `outbound_limits` field on the mailbox API using the keys `per_hour`, `per_day`, and `recipients_per_message`.

### Per-inbox spam thresholds

Each mailbox can override spam thresholds through `spam_thresholds` on `POST /api/v1/mailboxes` or `PATCH /api/v1/mailboxes/:id`. For example, this PATCH body sets the Junk score threshold to 3 and the TypeSafe spam probability threshold to 0.95:

```json
{
  "spam_thresholds": {
    "junk_threshold": 3,
    "spam_threshold": 0.95
  }
}
```

| Key | Allowed values | Inherited setting |
|-----|----------------|-------------------|
| `junk_threshold` | Finite number greater than or equal to 0 | `spam.junk_threshold`, default 5 |
| `spam_threshold` | Finite number greater than 0 and at most 1 | `spam.typesafe.spam_threshold`, default 0.98 |
| `phishing_threshold` | Finite number greater than 0 and at most 1 | `spam.typesafe.phishing_threshold`, default 0.98 |

Lower values send more messages to Junk; equality with a threshold also selects Junk. An explicit mailbox `junk_threshold` of 0 selects Junk even for a score of 0. TypeSafe thresholds take effect only when the server enables TypeSafe, and `shadow` mode records decisions without changing folders.

A supplied object replaces all overrides for that mailbox. Omitted keys inherit server settings. Send `{"spam_thresholds": null}` or `{"spam_thresholds": {}}` to restore full inheritance; omitting `spam_thresholds` from a PATCH leaves it unchanged. Mailbox GET and list responses include configured overrides when present. Changes apply to newly received mail without a restart and leave stored messages in their current folders.

Each recipient's mailbox determines initial Junk placement, including delivery through aliases and plus addresses. SMTP rejection remains server-wide. Mailbox thresholds do not override explicit sender blocks or DMARC quarantine, and routing rules still run after initial placement.

The message API records each inbox's effective Junk threshold, initial placement, reasons, and TypeSafe result under `auth_results.mailboxes[mailbox_id]`. Use these records for per-inbox decisions; the top-level `auth_results.typesafe` describes the server-default policy. TypeSafe request and token metrics count once per message, and decision counters indicate whether any recipient met a TypeSafe threshold.

### Optional TypeSafe content filter

`spam.typesafe.mode` defaults to `off`, which makes no external requests. `shadow` sends messages to TypeSafe and records what the filter would do without changing placement; `junk` additionally selects Junk when `spam_probability >= spam_threshold` or `phishing_probability >= phishing_threshold`. TypeSafe never sets an SMTP rejection and never changes `spam_score`; an outage or malformed response leaves the existing policy in control. Thresholds default to `0.98`, a starting point for shadow evaluation rather than a calibrated production value.

Enabling `shadow` or `junk` sends message content to `https://api.typesafe.ai/v1/systemone`. The request carries the decoded subject, sender display name and address, Reply-To, bounded plain-text and HTML text, link destinations stripped of credentials, query strings and fragments, bounded attachment filenames and content types, and a server-computed SPF/DKIM/DMARC summary. It does not send raw headers, recipient lists, Bcc, mailbox identifiers, attachment bytes, or fetched link/remote-image content. Confirm provider retention, training use, processing region, rate limits, and pricing before production enablement; redacting metadata does not remove personal information from the body.

Each inbound message triggers at most one request, bounded by `timeout_ms` after the concurrency and per-minute budgets are checked. Messages already rejected or classified as Junk for every recipient by the existing checks skip the request, as do empty or unparseable bodies. Results are stored under `auth_results.typesafe` in the message API response.

Set `HAITATSU_TYPESAFE_API_KEY` and change the example's `spam.typesafe.mode` to `shadow` to begin evaluation. Reload through SIGHUP or the admin reload endpoint. The default timeout is 2 seconds, with up to 8 requests in flight and 60 requests per minute per process. Divide the account budget across replicas. Omitted or zero numeric settings use defaults. Authentication and request-validation errors suppress further calls until the client sees changed settings; overload and repeated service failures use a bounded cooldown.

The extractor scans at most 2 MiB of raw mail, 32 MIME entities with a nesting limit, and 32 KiB of top-level headers. It reads up to 64 KiB of each first text alternative and retains a combined 16 KiB by default, reserving HTML evidence when both alternatives exist. Truncation and parse limitations appear in the stored result. Text after the scan limit, image-only messages and attachment contents may escape content detection.

Sender allow rules reduce the existing numeric score but do not bypass TypeSafe. Recipients share one content evaluation, with each mailbox applying its own thresholds. Mailbox routing rules run afterward and can move mail out of Junk. Changing to `off` stops new assessments without moving previously classified mail.

For threshold calibration, `go run ./cmd/typesafe-eval -corpus labels.jsonl > predictions.jsonl 2> summary.json` evaluates labeled local mail without delivering it. It requires the API key and an explicit JSONL manifest. See [the evaluation guide](docs/typesafe-evaluation.md) for the format, metrics, and rollout procedure. Automated tests use fake responses.

## API pagination

Every list endpoint accepts `limit` (max 100) and `cursor`. The response includes `pagination.next`, which is an opaque cursor to pass back for the following page. An empty `next` means the listing is complete.

## Multi-node

IMAP IDLE notifications and login throttling are shared between nodes. PostgreSQL uses `LISTEN/NOTIFY`; libSQL uses a short-lived database change log that is polled every 250 milliseconds. Local SQLite is intended for one Haitatsu process. PostgreSQL remains the better choice for sustained concurrent writes.

Stop the stack with `task compose:down`. Reset volumes with `task compose:reset`.

## Deployment

Cluster deployment lives in the `infra` repo; the image is built here and shipped to the machines with `uc image push` through unregistry. See `deploy/README.md`.

## Common tasks

| Task | Command |
|------|---------|
| Build binary | `task build` |
| Build Docker image | `task docker:build` |
| Print build version | `./haitatsu -version` |
| Start compose stack | `task compose:up` |
| Wipe database schema | `task db:wipe` |
| Run tests | `task test` |
| Regenerate ent code | `task generate` |
