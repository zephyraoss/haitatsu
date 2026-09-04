# Deploying

Haitatsu runs on the Uncloud cluster defined in the `infra` repo, under
`services/haitatsu/`. There is no image registry in the path: build the
image here with local Docker and push it to the replica machines through
the unregistry embedded in each Uncloud daemon, then pin that tag in the
stack with `pull_policy: never`:

```sh
docker build --build-arg VERSION=$(git describe --tags --always) -t haitatsu:<version> .
uc image push haitatsu:<version> -m bocchi,kita
```

The push needs the target machines' Docker on the containerd image store.
The container carries no config of its own: the stack declares
`haitatsu.pkl` as a file-based config mounted at
`/etc/haitatsu/haitatsu.pkl`, and the binary refuses to start if nothing is
mounted there.

The committed Pkl holds every non-secret value literally. Secrets come from
the container environment through `read("env:...")`, so the stack's
`environment` block maps each variable below to a `${...}` reference that
`bin/deploy` resolves from the stack's `.env` with `op run`.

| Variable | Used by |
|----------|---------|
| `HAITATSU_DATABASE_DRIVER` | `database.driver` (optional, defaults to `postgres`) |
| `HAITATSU_DATABASE_DSN` | `database.dsn` (falls back to `HAITATSU_POSTGRES_DSN`) |
| `HAITATSU_DATABASE_AUTH_TOKEN` | `database.auth_token` (libSQL only) |
| `HAITATSU_POSTGRES_DSN` | Legacy fallback for `database.dsn` |
| `HAITATSU_S3_ACCESS_KEY` | `s3.access_key_id` |
| `HAITATSU_S3_SECRET_KEY` | `s3.secret_access_key` |
| `HAITATSU_SERVICE_TOKEN` | `api.service_token` |
| `HAITATSU_RELAY_USERNAME` | `relay.username` |
| `HAITATSU_RELAY_PASSWORD` | `relay.password` |
| `HAITATSU_WEBHOOK_SECRET` | `webhooks.secret` |
| `HAITATSU_NOTIFICATION_RENDER_SECRET` | `notifications.render_secret` |
| `HAITATSU_CERTS_S3_ACCESS_KEY` | `tls.storage.access_key_id` (read-only key for the Caddy cert bucket) |
| `HAITATSU_CERTS_S3_SECRET_KEY` | `tls.storage.secret_access_key` |
| `HAITATSU_AXIOM_TOKEN` | `logging.axiom_token` (optional) |

`server.instance_name` reads `HOSTNAME`, which Docker sets to the container
ID, so every replica names itself without per-replica config.

The service needs `x-ports` for 25, 143, 465 and 587 in host mode, since
Uncloud's Caddy owns 80 and 443 on every machine. That is also why the
example config uses `tls.mode = "storage"` rather than `acme`: Caddy already
issues and renews certificates for every name in its site blocks and keeps
them in the `caddy-certs` bucket on Garage, so the stack lists the mail
hostnames in an `x-caddy` block and Haitatsu reads the resulting files out of
that bucket with a read-only key. Every replica serves the same certificate
and none of them ever talks to Let's Encrypt.

Uncloud recreates containers when a file-based config changes, so editing the
Pkl and pushing redeploys the service. The SIGHUP hot-reload path still works
for a manually edited file inside a running container, but it is not how
changes reach the cluster.
