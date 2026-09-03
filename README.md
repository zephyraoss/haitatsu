# Haitatsu

Haitatsu is a simple email server written in Go. It receives and stores mail, exposes IMAP and SMTP submission for clients, and provides a REST API for trusted backend services to manage mailboxes and messages.

## Features

- IMAP4rev1 with stable per-folder UIDs, IDLE push, server-side SEARCH, MOVE, UIDPLUS, ESEARCH, SPECIAL-USE, LIST-STATUS, and label folders under `Labels/`
- SMTP inbound with SPF, DKIM, DMARC (relaxed and strict alignment), DNSBL, sender allow/block lists, per-IP connection and rate limits
- SMTP submission with PLAIN and LOGIN auth, DKIM signing, Bcc stripping, per-mailbox outbound limits, send-as for routed aliases
- Relay delivery with exponential backoff over roughly two days, permanent failure notifications
- REST API with cursor pagination, constant-time service token auth
- TLS from automatic ACME certificates, or read straight out of a certmagic-layout S3 bucket that something else (such as Caddy) already keeps current, for any number of hostnames
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

The server reads config from `/etc/haitatsu/haitatsu.pkl` by default and refuses to start if nothing is there. Override with `-config path/to/haitatsu.pkl`. The Docker image ships no config, so mount one at that path.

Any field can read from the environment with `read("env:NAME")`, and `read?("env:NAME") ?? ""` makes it optional. The example config does this for every secret (Postgres DSN, S3 keys, service token, relay credentials, webhook secret, notification render secret, certificate-bucket keys, Axiom token) and sets `instance_name` from `HOSTNAME`, so a deployment is the committed file plus a handful of environment variables. See `deploy/README.md` for the list.

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

TLS has four modes. `manual` loads `cert_file` and `key_file`. `acme` obtains a certificate for `public_hostname` itself using HTTP-01 or TLS-ALPN-01 on the listener host, cached under `acme_cache_path` (default `/var/lib/haitatsu/certmagic`). `storage` issues nothing and instead reads certificates from the S3 bucket in `tls.storage`, in the layout certmagic writes (`<prefix>/certificates/<issuer>/<host>/<host>.crt` and `.key`), for `public_hostname` plus every name in `storage.hostnames`, re-reading them every `refresh_interval_minutes` (and every 30 seconds while any are still missing, so it can start before the issuer has produced them). Use `storage` when a reverse proxy such as Caddy already owns ports 80 and 443 and keeps a shared certificate store, so replicas serve the same certificate without any of them talking to the CA; the bucket credentials only need read access. `off` disables TLS and allows plaintext authentication, which is only for local development.

Per-mailbox outbound limits override the defaults through the `outbound_limits` field on the mailbox API using the keys `per_hour`, `per_day`, and `recipients_per_message`.

## API pagination

Every list endpoint accepts `limit` (max 100) and `cursor`. The response includes `pagination.next`, which is an opaque cursor to pass back for the following page. An empty `next` means the listing is complete.

## Multi-node

IMAP IDLE notifications and login throttling are shared between nodes through Postgres (`LISTEN/NOTIFY` and the `auth_lockouts` table), so any number of Haitatsu processes can serve the same database.

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
