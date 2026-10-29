# Gitea Pages

Gitea Pages is the publishing service for repository websites managed by Gitea. Gitea owns site configuration, publication sources, instance registration, and user-visible deployment status. Each Gitea Pages instance synchronizes that state, downloads the current `tar.gz` artifact, deploys it locally, serves the public site, and reports the result to Gitea.

The service supports two deployment modes:

- **Standalone** serves HTTP and HTTPS directly from the `gitea-pages` process.
- **Nginx** writes site files and a validated Nginx include file, then runs operator-configured test and reload commands. Nginx can keep serving the last confirmed configuration while the Pages process is restarting.

## Features

- GitHub-style project URLs at `https://<owner>.<pages-domain>/<repository>/` and owner sites selected by a repository named `.pages`.
- Active full and incremental synchronization with Gitea; normal web requests read only locally deployed content.
- Repository branch/directory, Actions artifact, and web/API artifact publication through the Gitea integration.
- Safe `tar.gz` extraction with path, entry count, expanded-size, depth, timeout, and link checks. Executable permission bits are removed from deployed files.
- Custom domains protected by Gitea and Pages TXT ownership checks.
- Operator-provided certificates or automated ACME certificates implemented with [`go-acme/lego`](https://github.com/go-acme/lego).
- HTTP-01 for custom domains and DNS-01 for the default Pages domain.
- Single-member embedded etcd for local durable state without a network listener.
- Custom index, 404, asset, and Nginx template resources.

Gitea Pages currently publishes public sites. Private repository access control is outside the current integration.

## Requirements

- Go 1.27 or later to build from source.
- A Gitea installation containing the Pages integration and an instance token created by a Gitea site administrator.
- A persistent local filesystem for Pages data and deployed sites.
- Nginx when running in `nginx` mode.
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

Use a service manager to provide the environment and keep the process running. `GITEA_PAGES_DATA_ROOT` contains the embedded etcd state, downloaded deployment data, active site directories, and route state, so it must use persistent storage.

## Configuration

All configuration is supplied through environment variables. Paths must be absolute, and the data, certificate, Nginx output, HTTP-01, and custom-resource roots must not overlap.

### Common settings

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `GITEA_PAGES_GITEA_URL` | Yes | — | HTTPS root URL of the Gitea server, without a path. |
| `GITEA_PAGES_ROOT_URL` | Yes | — | Public HTTP or HTTPS Pages root URL on the standard scheme port. |
| `GITEA_PAGES_TOKEN` | Yes | — | Instance communication token created in Gitea administration. |
| `GITEA_PAGES_DATA_ROOT` | Yes | — | Persistent root for embedded etcd, deployments, temporary files, and route state. |
| `GITEA_PAGES_SYNC_INTERVAL` | No | `1m` | Interval between incremental synchronizations; the minimum is one minute. |
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
| `GITEA_PAGES_HTTP01_PROBE_ADDRESSES` | No | Comma-separated IP addresses used to resolve custom hosts during HTTP-01 preflight checks. |
| `GITEA_PAGES_DNS_PROVIDER` | For default-domain DNS-01 | A DNS provider supported by lego, or `command` for the command adapter. |
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

When `GITEA_PAGES_DNS_PROVIDER` names a built-in lego provider, configure that provider with its [documented environment variables](https://go-acme.github.io/lego/dns/). The command adapter is useful for integrating an operator-managed DNS tool without adding provider-specific code to Gitea Pages.

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

Custom domains become eligible only after the configured ownership TXT record succeeds. Pages repeats that check locally and performs an HTTP-01 reachability preflight before asking the ACME authority to validate a custom domain. Gitea stores deployment state and results; ACME account keys, certificate private keys, and DNS credentials stay on the Pages instance.

## License

This project is licensed under the [MIT License](LICENSE).
