# Registry

Registry is a small container registry with a web dashboard. One Go binary holds the Docker registry ([distribution](https://github.com/distribution/distribution) v3) and the dashboard.

The dashboard shows repositories, tags, layers, storage use, traffic, and recent activity. You can delete tags and repositories, run garbage collection, make a repository public or private, and create access tokens.

## Run

```bash
docker build -t registry .
```

```bash
docker run -d --name registry -p 5000:5000 -v registry-data:/data -e PUBLIC_URL=https://registry.example.com registry
```

To run it without Docker, use Go 1.27.2 or later:

```bash
go run .
```

## Create the account

The registry has one account. You create it on the first visit.

1. Read the setup code in the server log, for example with `docker logs registry`.
2. Open `/setup` in a browser.
3. Enter the setup code, a username, and a password.

## Tokens

You do not use your password with `docker login`. You use an access token, as on GitHub.

1. Open the dashboard and click Tokens.
2. Give the token a name, an access level, and an expiry date.
3. Copy the token. The dashboard shows it one time only.
4. Sign in with the token as the password:

```bash
docker login registry.example.com -u <username>
```

A pull token reads repositories. A push token also pushes and deletes. You can limit a token to some repositories, for example `team/api, tools/*`. The pattern `tools/*` matches all repositories in `tools`.

## Public and private repositories

A new repository is private. A private repository needs a token for each pull. A public repository allows pulls without a token. A push always needs a token with push access.

## Garbage collection

If you delete a tag, the layers stay on disk. Run garbage collection from the dashboard to free the space. Garbage collection also deletes manifests that have no tag. During the run, the registry rejects pushes.

## Backups

The registry can keep a backup of the newest image of each repository in an S3 bucket. The newest image is the last tag that you pushed, together with the other tags on the same image. The backup also holds the dashboard data: the account, the tokens, and the public or private state of each repository.

The backup runs 15 seconds after a push, for that repository only. The server also backs up all repositories at start. The Back up now button on the storage card does the same. When a newer image replaces an older one, the backup removes the older image and its layers. A repository that you delete stays in the backup.

To turn on backups, set `backup.s3.bucket` and the other `backup.s3` keys. The bucket must be private, because it holds the token hashes.

The bucket uses the normal registry layout. To restore, start the registry with the backup bucket as its storage:

```bash
STORAGE_DRIVER=s3 S3_BUCKET=registry-backup S3_REGION=eu-central-1 S3_PREFIX=registry ./registry
```

The images, the account, and the tokens are then available again. To move the images back to disk storage, copy them with a tool such as `crane copy`.

## HTTPS and CDNs

The server does not need TLS. It serves plain HTTP unless you set `TLS_CERT` and `TLS_KEY`. Docker needs HTTPS for a registry that is not on `localhost`. A reverse proxy or a CDN in front of the registry can supply HTTPS.

If a proxy or a CDN is in front of the registry, set `PUBLIC_URL` to the external URL. Docker gets its tokens from `PUBLIC_URL/token`.

Before you put the registry behind a CDN, read these limits:

- Turn off caching for `/v2/` and `/token`. A cached blob from a private repository goes to clients that do not have a token.
- Make sure that the CDN accepts large request bodies. Docker sends each layer in one request. Cloudflare Free and Pro plans reject bodies over 100 MB, so a push of a large layer fails.
- Make sure that the CDN timeout is long enough for your largest layer.

### CloudFront

CloudFront accepts a request body up to 64 GB, so the size of a layer is not a problem. The origin response timeout is 30 seconds by default. You can set it up to 120 seconds, and you can ask AWS for a higher quota. After a timeout on a PUT or PATCH request, CloudFront does not try again. Test a push of your largest layer before you rely on it.

For the `/v2/*` and `/token` paths, use this configuration:

- The `CachingDisabled` cache policy.
- An origin request policy that forwards all viewer headers, so that the `Authorization` header gets to the registry.
- All HTTP methods allowed.

CloudFront sends only the first of several `Accept` headers with the same name to the origin. Docker sends each manifest type in its own `Accept` header, in a random order. The registry adds all manifest types to each manifest read, so pulls through CloudFront work. The registry always returns the manifest as it was pushed.

## Kubernetes

The Helm chart is in [charts/registry](charts/registry). It runs one replica, because the server keeps the dashboard state in memory.

Each release puts the chart in GHCR, next to the image. Replace `<owner>` with the GitHub user or organization:

```bash
helm install registry oci://ghcr.io/<owner>/charts/registry --version 0.1.0 --set ingress.enabled=true --set ingress.host=registry.example.com --set ingress.tlsSecret=registry-tls
```

To install from a clone of this repository, also set the image:

```bash
helm install registry ./charts/registry --set image.repository=ghcr.io/<owner>/registry
```

After the install, the chart prints the steps to read the setup code.

The values under `config` go into the configuration file as they are. If the ingress is on and `config.server.public_url` is empty, the chart sets it to `https://` and the ingress host.

The default ingress annotations are for ingress-nginx. They remove the body size limit, turn off request buffering, and set the timeouts to 15 minutes.

With the filesystem driver, the chart creates a volume claim of 50 GiB. The claim stays after `helm uninstall`, so you do not lose images by accident.

To use S3, set `config.storage.driver` to `s3` and set the bucket under `config.storage.s3`. For the credentials, do one of these:

- Set `s3.accessKey` and `s3.secretKey`. The chart puts them in a secret.
- Set `s3.existingSecret` to a secret that has the keys `accessKey` and `secretKey`.
- Leave both empty and use an IAM role. For IRSA, add the role ARN to `serviceAccount.annotations`.

## Releases

GitHub Actions run two workflows:

- CI checks the format, runs `go vet`, the tests, golangci-lint, and `helm lint` on each push and pull request.
- Release builds the image for `linux/amd64` and `linux/arm64`. For a push to `main`, it pushes the image to GHCR with the tags `main` and the commit SHA. For a pull request, it only builds the image.

To make a release, push a version tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The release workflow then pushes the image as `ghcr.io/<owner>/<repo>:0.1.0` and the chart as `oci://ghcr.io/<owner>/charts/registry` version `0.1.0`. In the published chart, the default image is that GHCR image.

New GHCR packages are private. To let others pull them, open each package on GitHub and make it public.

## Configuration

The built-in defaults are in the file `config.yaml`. Each value reads an environment variable, so you can configure the server with environment variables only.

To use a file, give its path with `-config` or with the `CONFIG_FILE` variable. The values in your file replace the defaults. A value in your file can also read an environment variable, with a default after the colon:

```yaml
server:
  public_url: https://registry.example.com
log:
  format: console
storage:
  driver: s3
  s3:
    bucket: my-registry
    region: eu-central-1
    secret_key: ${S3_SECRET_KEY:""}
```

If a key is unknown or a value is not valid, the server stops at start.

| Key | Variable | Default | Meaning |
| --- | --- | --- | --- |
| `server.addr` | `ADDR` | `:5000` | The address that the server listens on. |
| `server.public_url` | `PUBLIC_URL` | From the request | The external URL, for example `https://registry.example.com`. |
| `server.tls_cert`, `server.tls_key` | `TLS_CERT`, `TLS_KEY` | Empty | Certificate and key files for HTTPS. |
| `log.level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `log.format` | `LOG_FORMAT` | `json` | `json` or `console`. |
| `storage.driver` | `STORAGE_DRIVER` | `filesystem` | `filesystem` or `s3`. |
| `storage.filesystem.root` | `DATA_DIR` | `./data`, `/data` in the image | The folder for images and dashboard data. |
| `storage.s3.bucket` | `S3_BUCKET` | Empty | The bucket name. |
| `storage.s3.region` | `S3_REGION` | Empty | The bucket region, for example `eu-central-1`. |
| `storage.s3.endpoint` | `S3_ENDPOINT` | Empty | The endpoint for an S3-compatible service, for example MinIO. |
| `storage.s3.access_key`, `storage.s3.secret_key` | `S3_ACCESS_KEY`, `S3_SECRET_KEY` | Empty | Credentials. If they are empty, the AWS default credential chain applies. |
| `storage.s3.prefix` | `S3_PREFIX` | Empty | A folder in the bucket. |
| `storage.s3.force_path_style` | `S3_FORCE_PATH_STYLE` | `false` | Set to `true` for MinIO and most S3-compatible services. |

With S3, the dashboard keeps its data in the same bucket, under `dashboard/`. You do not need a volume.

The `backup.s3` keys have the same meaning as the `storage.s3` keys. Their variables start with `BACKUP_S3_`, for example `BACKUP_S3_BUCKET`. If `backup.s3.bucket` is empty, backups are off. The backup bucket and prefix must differ from the storage bucket and prefix.

In the Helm chart, set the backup keys under `config.backup.s3`. Put the credentials in `backup.accessKey` and `backup.secretKey`, or set `backup.existingSecret` to a secret that has the keys `accessKey` and `secretKey`.

## Logs

The server writes logs to standard error. It uses the [zap](https://github.com/uber-go/zap) library.

Each push, pull, delete, and garbage collection run gives one `info` line with the repository, the reference, and the token name. Token changes, visibility changes, and failed sign-ins also give one line each.

The logs of the embedded registry go through the same logger, with the name `distribution`. Requests that end with a 4xx status are at `debug` level.

The icons come from [Lucide](https://lucide.dev) (ISC license).
