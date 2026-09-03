# Deploying

Haitatsu runs on the Uncloud cluster defined in the `infra` repo, under
`services/haitatsu/`. There is no image registry in the path: the stack's
service uses `build:` pointing at this repo, and `uc deploy` builds the
Dockerfile locally and pushes the resulting image straight to the cluster
machines through unregistry. Pass `VERSION` as a build arg so the binary
reports the tag it was built from. The container carries no config of its
own: the stack declares `haitatsu.pkl` as a file-based config mounted at
`/etc/haitatsu/haitatsu.pkl`, and the binary refuses to start if nothing is
mounted there.

The committed Pkl holds every non-secret value literally. Secrets come from
the container environment through `read("env:...")`, so the stack's
`environment` block maps each variable below to a `${...}` reference that
`bin/deploy` resolves from the stack's `.env` with `op run`.

| Variable | Used by |
|----------|---------|
| `HAITATSU_POSTGRES_DSN` | `postgres.dsn` |
| `HAITATSU_S3_ACCESS_KEY` | `s3.access_key_id` |
| `HAITATSU_S3_SECRET_KEY` | `s3.secret_access_key` |
| `HAITATSU_SERVICE_TOKEN` | `api.service_token` |
| `HAITATSU_RELAY_USERNAME` | `relay.username` |
| `HAITATSU_RELAY_PASSWORD` | `relay.password` |
| `HAITATSU_WEBHOOK_SECRET` | `webhooks.secret` |
| `HAITATSU_NOTIFICATION_RENDER_SECRET` | `notifications.render_secret` |
| `HAITATSU_CLOUDFLARE_API_TOKEN` | `tls.acme_cloudflare_api_token` |
| `HAITATSU_AXIOM_TOKEN` | `logging.axiom_token` (optional) |

`server.instance_name` reads `HOSTNAME`, which Docker sets to the container
ID, so every replica names itself without per-replica config.

The service needs `x-ports` for 25, 143, 465 and 587 in host mode, since
Uncloud's Caddy owns 80 and 443 on every machine. That is also why the
example config uses the Cloudflare DNS-01 challenge with certificates stored
in the Garage bucket: replicas share one certificate instead of each issuing
its own, and none of them needs to answer on a port Caddy already holds.

Uncloud recreates containers when a file-based config changes, so editing the
Pkl and pushing redeploys the service. The SIGHUP hot-reload path still works
for a manually edited file inside a running container, but it is not how
changes reach the cluster.
