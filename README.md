# Gitea Pages

Gitea Pages is the publishing service for repository websites managed by Gitea. Gitea owns site configuration, publication sources, instance registration, and user-visible deployment status. Each Gitea Pages instance synchronizes that state, downloads the current `tar.gz` artifact, deploys it locally, serves the public site, and reports the result to Gitea.

The service supports two deployment modes:

- **Standalone** serves HTTP and HTTPS directly from the `gitea-pages` process.
- **Nginx** writes site files and a validated Nginx include file, then runs operator-configured test and reload commands. Nginx can keep serving the last confirmed configuration while the Pages process is restarting.

## Features

- GitHub-style project URLs at `https://<owner>.<pages-domain>/<repository>/` and owner sites selected by a repository named `.pages`, with one address calculation shared by both serving modes and publication probes.
- Active full and incremental synchronization with Gitea; normal web requests read only locally deployed content.
- Repository branch/directory, Actions artifact, and web/API artifact publication through the Gitea integration.
- Safe `tar.gz` extraction with Gitea-synchronized entry count and size limits, plus fixed path, depth, timeout, and link checks. Executable permission bits are removed from deployed files.
- Custom domains protected by independent Gitea and Pages TXT ownership checks when enabled in Gitea's `[pages]` configuration. A domain that has succeeded keeps a 24-hour grace period after later DNS failures.
- Operator-provided certificates or automated ACME certificates implemented with [`go-acme/lego`](https://github.com/go-acme/lego).
- HTTP-01 for custom domains and DNS-01 for the default Pages domain.
- Embedded etcd for local durable state without a network listener.
- Custom index, 404, asset, and Nginx template resources.

Every enabled Pages site is public. Repository visibility and website access are separate settings.

## Requirements

- Go 1.27 or later to build from source.
- A Gitea installation containing the Pages integration and an instance token created by a Gitea site administrator.
- A persistent local filesystem for Pages data and deployed sites.
- Nginx 1.19.4 or later when running in `nginx` mode.
- DNS and ACME credentials when Pages manages certificates for the default domain.

## Build and test

```sh
make check
make build
./bin/gitea-pages --help
```

`make check` runs Go vet, the complete test suite, and a build. The binary is written to `bin/gitea-pages`.

## Quick start

Register a Pages instance in Gitea site administration and copy its communication token. For a plain HTTP standalone instance:

```sh
export GITEA_PAGES_GITEA_URL=https://gitea.example.com
export GITEA_PAGES_ROOT_URL=http://pages.example.com
export GITEA_PAGES_TOKEN='replace-with-the-instance-token'
export GITEA_PAGES_DATA_ROOT=/var/lib/gitea-pages
export GITEA_PAGES_HTTP_LISTEN=:80

./bin/gitea-pages standalone
```

After the instance registers successfully, enable Pages in a repository's settings and select its publication source. The first synchronization loads every enabled site; later synchronizations request only entries changed after the cursor returned by Gitea.

Each synchronization response also supplies the archive limits and the custom-domain TXT requirement configured by Gitea. The instance validates and stores those values in embedded etcd before using them for new downloads and deployments. Changing a limit does not redeploy already published content; the next content update is checked with the new value.

New and changed sites are queued as soon as their synchronized facts are durable. When the bounded queue fills, one background scanner keeps feeding pending repository IDs as workers free space. Startup recovery and a five-minute recovery scan rediscover work left by a restart or failed deployment, without scanning every site on each normal synchronization.

Use a service manager to provide the environment and keep the process running. `GITEA_PAGES_DATA_ROOT` contains the embedded etcd state, downloaded deployment data, active site directories, and route state, so it must use persistent storage.

A completed full synchronization replaces the local site snapshot. Embedded etcd records an in-progress marker while that snapshot is persisted. If the process stops during the replacement, the next start keeps the expanded content directories, ignores the incomplete site records, and performs a new full synchronization before rebuilding routes.

## Configuration

All configuration is supplied through environment variables. Filesystem paths are absolute so service behavior does not depend on the process working directory. Data, certificate, Nginx output, HTTP-01, and custom-resource roots are separate because Pages replaces and cleans files inside the roots it owns.

### Common settings

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `GITEA_PAGES_GITEA_URL` | Yes | — | HTTP or HTTPS root URL of the Gitea server, without a path. |
| `GITEA_PAGES_ROOT_URL` | Yes | — | Public HTTP or HTTPS Pages root URL on the standard scheme port. |
| `GITEA_PAGES_TOKEN` | Yes | — | Instance communication token created in Gitea administration. |
| `GITEA_PAGES_DATA_ROOT` | Yes | — | Persistent root for embedded etcd, deployments, temporary files, and route state. |
| `GITEA_PAGES_SYNC_INTERVAL` | No | `1m` | Interval between incremental synchronizations; the minimum is one minute. |
| `GITEA_PAGES_DNS_RESOLVER` | No | System resolver | Recursive DNS server for local custom-domain TXT checks, as an IPv4 or IPv6 address with an optional port. Port 53 is used when omitted. |
| `GITEA_PAGES_DNS_REFRESH_INTERVAL` | No | `15m` | Local TXT cache and active refresh interval, from one minute through 24 hours. |
| `GITEA_PAGES_DEPLOY_TIMEOUT` | No | `10m` | Maximum time for one deployment operation. |
| `GITEA_PAGES_DEPLOY_QUEUE_SIZE` | No | `32` | Maximum number of queued deployment jobs. |
| `GITEA_PAGES_DEPLOY_CONCURRENCY` | No | `2` | Number of deployments processed concurrently. |
| `GITEA_PAGES_ETCD_QUOTA_BYTES` | No | `2147483648` | Embedded etcd backend quota; the minimum is 64 MiB. |
| `GITEA_PAGES_CUSTOM_ROOT` | No | — | Existing directory containing replacement platform resources. |

### Standalone mode

| Variable | Required | Purpose |
| --- | --- | --- |
| `GITEA_PAGES_HTTP_LISTEN` | Yes | HTTP listen address. It also serves HTTP-01 challenges for HTTPS instances. |
| `GITEA_PAGES_HTTPS_LISTEN` | For an HTTPS root URL | HTTPS listen address. |

Start the service with:

```sh
./bin/gitea-pages standalone
```

### Nginx mode

| Variable | Required | Purpose |
| --- | --- | --- |
| `GITEA_PAGES_NGINX_ROOT` | Yes | Output root for `pages.conf`, public resources, certificate exports, and checked configuration snapshots. |
| `GITEA_PAGES_NGINX_HTTP01_ROOT` | For an HTTPS root URL | Directory exposed by Nginx for HTTP-01 challenge files. |
| `GITEA_PAGES_NGINX_HTTP_PROBE_ADDR` | Yes | Local IP and port used to verify the HTTP virtual host. |
| `GITEA_PAGES_NGINX_HTTPS_PROBE_ADDR` | For an HTTPS root URL | Local IP and port used to verify the HTTPS virtual host. |
| `GITEA_PAGES_NGINX_TEST_COMMAND` | Yes | JSON string array for the Nginx configuration test command. It must contain `{candidate}` exactly once. |
| `GITEA_PAGES_NGINX_RELOAD_COMMAND` | Yes | JSON string array for the Nginx reload command. |
| `GITEA_PAGES_NGINX_COMMAND_TIMEOUT` | No | Command timeout; defaults to `30s`. |

`pages.conf` is an include fragment for the Nginx `http` context. The test command must construct or select a complete Nginx configuration that includes the file referenced by `{candidate}`. The reload command must make the running Nginx process read the confirmed `pages.conf` from `GITEA_PAGES_NGINX_ROOT`.

For example, with an operator-provided wrapper that accepts the candidate path:

```sh
export GITEA_PAGES_NGINX_TEST_COMMAND='["/usr/local/libexec/gitea-pages-nginx-test","{candidate}"]'
export GITEA_PAGES_NGINX_RELOAD_COMMAND='["nginx","-s","reload"]'

./bin/gitea-pages nginx
```

The Nginx worker must be able to traverse the `sites` and `route-state` directories below `GITEA_PAGES_DATA_ROOT`. The embedded etcd directory remains private to the Pages process.

### HTTPS and ACME

| Variable | Required | Purpose |
| --- | --- | --- |
| `GITEA_PAGES_CERT_ROOT` | No | Operator certificate input root, separate from all other Pages directories. |
| `GITEA_PAGES_ACME_DIRECTORY_URL` | To enable ACME | ACME directory endpoint. |
| `GITEA_PAGES_ACME_EMAIL` | With ACME | Contact email for the ACME account. |
| `GITEA_PAGES_ACME_EAB_KID` | With EAB | External Account Binding key identifier. |
| `GITEA_PAGES_ACME_EAB_HMAC_KEY` | With EAB | Base64url-encoded External Account Binding HMAC key. |
| `GITEA_PAGES_HTTP01_PROBE_ADDRESSES` | No | Comma-separated IP:port addresses used to reach custom hosts during HTTP-01 preflight checks. |
| `GITEA_PAGES_DNS_PROVIDER` | For default-domain DNS-01 | Set to `command`; Pages delegates TXT changes to the configured commands while lego handles ACME. |
| `GITEA_PAGES_ACME_DNS_RESOLVER` | No | `GITEA_PAGES_DNS_RESOLVER` | Recursive DNS server used by lego for DNS-01 zone discovery and propagation checks. |
| `GITEA_PAGES_DNS_PRESENT_COMMAND` | With the command adapter | JSON string array containing `{name}` and `{value}` exactly once. |
| `GITEA_PAGES_DNS_CLEANUP_COMMAND` | With the command adapter | JSON string array containing `{name}` and `{value}` exactly once. |

The default operator certificate is read from:

```text
<GITEA_PAGES_CERT_ROOT>/default/fullchain.pem
<GITEA_PAGES_CERT_ROOT>/default/privkey.pem
```

Custom operator certificates use a directory named with the lowercase host's SHA-256 digest:

```text
<GITEA_PAGES_CERT_ROOT>/hosts/<sha256>/host
<GITEA_PAGES_CERT_ROOT>/hosts/<sha256>/fullchain.pem
<GITEA_PAGES_CERT_ROOT>/hosts/<sha256>/privkey.pem
```

The `host` file contains the lowercase domain name. In Nginx mode, accepted certificates are exported below `<GITEA_PAGES_NGINX_ROOT>/certs` for the generated configuration.

The command adapter lets an operator connect the default-domain DNS-01 flow to an existing DNS tool without giving Pages provider-specific credentials or importing every lego DNS provider. The provider commands write and remove challenge records; `GITEA_PAGES_ACME_DNS_RESOLVER` controls lego's DNS reads and does not change provider behavior.

## Custom resources

Set `GITEA_PAGES_CUSTOM_ROOT` to an existing directory to replace bundled platform resources. The directory can contain:

```text
public/index.html
public/errors/404.html
public/assets/
nginx/pages.conf.tmpl
nginx/server.inc
nginx/location.inc
```

Bundled resources remain in use for files that are not overridden. This keeps site content separate from operator-controlled error pages, assets, and Nginx rendering templates.

## Security model

Publication artifacts are transported as `tar.gz`. Gitea validates the uploaded archive before storing it, and each Pages instance validates it again while extracting. Absolute paths, traversal paths, unsafe entry types, expanding archives that exceed limits, and links that resolve outside the deployment are rejected. Relative symbolic links are accepted when their resolved path stays inside the site directory, including links whose target is not present in the artifact. Deployed regular files have executable bits removed.

Custom domains become eligible only after the configured ownership TXT record succeeds. Pages repeats that check locally. After a success, later mismatches, missing records, and temporary resolver failures retain the current route for up to 24 hours from the last success; a domain that has never succeeded is not published. Pages also performs an HTTP-01 reachability preflight before asking the ACME authority to validate a custom domain. Gitea stores deployment state and results; ACME account keys, certificate private keys, and DNS credentials stay on the Pages instance.

## License

This project is licensed under the [MIT License](LICENSE).
