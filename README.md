# Haitatsu

Haitatsu is a simple email server written in Go. It receives and stores mail, exposes IMAP and SMTP submission for clients, and provides a REST API for trusted backend services to manage mailboxes and messages.

## Features

- IMAP support
- SMTP support (inbound, outbound is via relay)
- REST API
- Automatic ACME certificates
- App passwords for protocol access.
- PostgreSQL for metadata, S3-compatible storage for message blobs
- Pkl-based hot-reloadable configuratiion.

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

Stop the stack with `task compose:down`. Reset volumes with `task compose:reset`.

## Common tasks

| Task | Command |
|------|---------|
| Build binary | `task build` |
| Build Docker image | `task docker:build` |
| Start compose stack | `task compose:up` |
| Wipe database schema | `task db:wipe` |
