# Haitatsu

Haitatsu is a simple email server written in Go. It receives and stores mail, exposes IMAP and SMTP submission for clients, and provides a REST API for trusted backend services to manage mailboxes and messages.

## Features

- IMAP4rev1 with stable per-folder UIDs, IDLE push, server-side SEARCH, MOVE, UIDPLUS, ESEARCH, SPECIAL-USE, LIST-STATUS, and label folders under `Labels/`
- SMTP inbound with SPF, DKIM, DMARC (relaxed and strict alignment), DNSBL, sender allow/block lists, per-IP connection and rate limits
- SMTP submission with PLAIN and LOGIN auth, DKIM signing, Bcc stripping, per-mailbox outbound limits, send-as for routed aliases
- Relay delivery with exponential backoff over roughly two days, permanent failure notifications
- REST API with cursor pagination, constant-time service token auth
- Automatic ACME certificates
- App passwords for protocol access, with login throttling shared across nodes through Postgres
- Quota accounting that tracks deletes and expunges, with a recompute endpoint
- PostgreSQL for metadata, S3-compatible storage for message blobs, versioned migrations
- Pkl configuration with hot reload of spam, relay, webhook, notification, API token, and limits settings

Mailbox users do not call the REST API directly. Integrate from your own backend using a configured service token. End users authenticate to IMAP and SMTP with app passwords.

## Requirements

- Go 1.26 or later
- [Pkl](https://pkl-lang.org/) (for local runs outside Docker)
- PostgreSQL
- S3-compatible object storage (MinIO works for development)

## Configuration

Copy the example config and edit it for your environment:

```sh
cp haitatsu.example.pkl haitatsu.pkl
```

The server reads config from `haitatsu.pkl` by default. Override with `-config path/to/haitatsu.pkl`.

## Local development

Start Postgres and MinIO, then build and run Haitatsu:

```sh
docker compose up -d postgres minio minio-init
task build
./haitatsu -config haitatsu.pkl
```

Or run the full stack (Postgres, MinIO, and Haitatsu in Docker):

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

Listener addresses, Postgres, S3, TLS, and worker enablement require a restart. Everything else reloads on `SIGHUP` or `POST /api/v1/admin/reload`.

| Block | Keys |
|-------|------|
| `limits` | `max_message_size_bytes`, `max_inbound_recipients`, `max_submission_recipients`, `max_connections_per_ip`, `inbound_messages_per_minute_per_ip`, `default_outbound_per_hour`, `default_outbound_per_day`, `default_outbound_recipients_per_message` |
| `relay` | `addr`, `username`, `password`, `from_host`, `max_attempts`, `max_retry_minutes` |
| `webhooks` | `default_timeout_seconds`, `secret`, `endpoints`, `max_attempts` |
| `spam` | `junk_threshold`, `reject_threshold`, `dnsbl_zones`, `dnsbl_score`, `require_helo` |
| `imap` | `addr`, `max_connections_per_ip` |

Per-mailbox outbound limits override the defaults through the `outbound_limits` field on the mailbox API using the keys `per_hour`, `per_day`, and `recipients_per_message`.

## API pagination

Every list endpoint accepts `limit` (max 100) and `cursor`. The response includes `pagination.next`, which is an opaque cursor to pass back for the following page. An empty `next` means the listing is complete.

## Multi-node

IMAP IDLE notifications and login throttling are shared between nodes through Postgres (`LISTEN/NOTIFY` and the `auth_lockouts` table), so any number of Haitatsu processes can serve the same database.

Stop the stack with `task compose:down`. Reset volumes with `task compose:reset`.

## Common tasks

| Task | Command |
|------|---------|
| Build binary | `task build` |
| Build Docker image | `task docker:build` |
| Start compose stack | `task compose:up` |
| Wipe database schema | `task db:wipe` |
| Run tests | `task test` |
| Regenerate ent code | `task generate` |
