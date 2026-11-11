# EdgeCDN-X ExternalDNS webhook

An ExternalDNS webhook provider that reads and manages DNS endpoints through
[`edgecdnx-sdk-go`](https://github.com/EdgeCDN-X/edgecdnx-sdk-go). It implements
the ExternalDNS webhook v1 protocol on port `8888`.

The provider uses the Project's configured Zones as its domain filters. It reads
each Zone's DNS endpoints and supports `A`, `AAAA`, `CNAME`, `TXT`, `MX`, `SRV`,
and `NS` records with Simple routing. EdgeCDN-X service-generated host-alias
endpoints (`LINK:` targets) are omitted because they are read-only views, not
DNS endpoint resources.

## Requirements and permissions

- An EdgeCDN-X Project with at least one Zone.
- An API key scoped to that Project with Zone `read` and `update` permissions.
- ExternalDNS configured to use its webhook provider and to reach this process.

DNS changes are translated to the Project API:

| ExternalDNS operation | EdgeCDN-X API operation |
| --- | --- |
| `GET /` negotiation | SDK `Zones.List`; return each zone as a suffix filter |
| `GET /records` | SDK `DNSRecords.List` for each zone |
| `POST /records` create | SDK `DNSRecords.Create` |
| `POST /records` update | SDK `DNSRecords.Get`, then `DNSRecords.Update` with `resourceVersion` |
| `POST /records` delete | SDK `DNSRecords.Get`, then `DNSRecords.DeleteWithOptions` with `resourceVersion` |

Updates and deletes use optimistic concurrency and retry conflicts a bounded
number of times. Repeated creates are accepted only when the existing endpoint
already matches. A record with a non-Simple routing policy cannot be updated or deleted by
ExternalDNS because the ExternalDNS endpoint model cannot represent the
EdgeCDN-X route selector; the provider returns a permanent `400` rather than
silently replacing that routing configuration.

## Credentials and configuration

The process requires these environment variables:

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `EDGECDNX_API_URL` | yes | — | Absolute EdgeCDN-X API URL, for example `https://api.edgecdnx.com` |
| `EDGECDNX_PROJECT_ID` | yes | — | Project metadata name |
| `EDGECDNX_API_KEY` | yes | — | Project-scoped API key |
| `WEBHOOK_LISTEN_ADDRESS` | no | `0.0.0.0:8888` | HTTP listen address |
| `LOG_LEVEL` | no | `info` | Log level: `debug`, `info`, `warn`, or `error` |

For example:

```sh
EDGECDNX_API_URL=https://api.edgecdnx.com \
EDGECDNX_PROJECT_ID=my-project \
EDGECDNX_API_KEY="$(cat /path/to/api-key)" \
./edgecdnx-external-dns-webhook
```

Use a secret manager or Kubernetes Secret for the API key; never put it in
Helm values, command-line arguments, logs, or a committed Secret manifest.
The published Go SDK is pinned to v0.1.1. Redirects are rejected, TLS
certificate validation is enabled, and outgoing API requests have a 10-second
timeout. The API key is sent only to the configured EdgeCDN-X API URL.

### Debug logging

Set `LOG_LEVEL=debug` to log incoming ExternalDNS webhook request details
(method, path, status, duration, decoded change sets). Health probe requests to
`/healthz` are not logged. EdgeCDN-X API requests and responses are also logged
at debug level; authorization headers are never logged and occurrences of the
API key in logged text are redacted. API error response bodies are not included
in webhook errors. DNS endpoint data appears in debug logs; it is public DNS
data, not credentials.

## ExternalDNS deployment

The ExternalDNS webhook specification recommends running the provider as a
sidecar and listening only on localhost. For a separate in-cluster deployment,
the default `0.0.0.0:8888` listen address allows ExternalDNS pods to reach the
webhook through its Service; restrict access with NetworkPolicy. Configure the
ExternalDNS container with:

```yaml
args:
  - --provider=webhook
  - --webhook-provider-url=http://localhost:8888
```

For a sidecar, set `WEBHOOK_LISTEN_ADDRESS=127.0.0.1:8888` instead. Ensure the
ExternalDNS deployment has the Project's API key in a Secret reference.

A Helm chart is published in the
[EdgeCDN-X Helm charts repository](https://github.com/EdgeCDN-X/helm-charts/tree/main/charts/edgecdnx-external-dns-webhook)
for deployments where a separate in-cluster service is required. Its
NetworkPolicy restricts ingress to the configured ExternalDNS namespace and pod
labels by default; verify that your CNI enforces NetworkPolicy before using
this mode.

Add the chart repository and install the chart after creating the API key
Secret:

```sh
helm repo add edgecdnx https://edgecdn-x.github.io/helm-charts/
helm repo update
helm upgrade --install edgecdnx-external-dns-webhook \
  edgecdnx/edgecdnx-external-dns-webhook \
  --namespace external-dns --create-namespace \
  --set edgecdnx.projectID=my-project \
  --set edgecdnx.apiURL=https://api.edgecdnx.com \
  --set edgecdnx.apiKeySecretRef.name=edgecdnx-api-key
```

Set `externalDNS.namespace` and `externalDNS.podSelector` to the namespace and
labels on the ExternalDNS pods. The default selector is
`app.kubernetes.io/name=external-dns` in namespace `kube-system`.

## Routing policies via annotations

Beyond plain `Simple` records, the webhook can provision the full EdgeCDN-X
routing stack (DNSEndpoint + Location + optional HealthCheckProfile) from
Ingress/Route annotations. See
[docs/routing-policies-plan.md](docs/routing-policies-plan.md) for the design
and multi-cluster walkthroughs.

```yaml
metadata:
  annotations:
    external-dns.alpha.kubernetes.io/set-identifier: k8s-fra
    external-dns.alpha.kubernetes.io/webhook-edgecdnx-routing: geo
    external-dns.alpha.kubernetes.io/webhook-edgecdnx-continents: EU
    external-dns.alpha.kubernetes.io/webhook-edgecdnx-countries: DE,AT,CH
    external-dns.alpha.kubernetes.io/webhook-edgecdnx-health-endpoint: /healthz
    external-dns.alpha.kubernetes.io/webhook-edgecdnx-health-protocol: https
    external-dns.alpha.kubernetes.io/webhook-edgecdnx-health-port: "443"
```

| Annotation (suffix of `external-dns.alpha.kubernetes.io/webhook-`) | Value |
|---|---|
| `edgecdnx-routing` | `simple` (default), `geo`, `wrr`, `rr`, `failover` |
| `edgecdnx-weight` | required for `wrr`: integer `0..1000` |
| `edgecdnx-continents` | `geo`: comma-delimited continent codes (`EU,NA`, ...) |
| `edgecdnx-countries` | `geo`: comma-delimited ISO country codes (`SK,HU`, ...) |
| `edgecdnx-fallback` | `failover`: fallback location name (required for primary) |
| `edgecdnx-failover-role` | `failover`: `primary` (default) or `secondary` |
| `edgecdnx-health-endpoint` | HTTP(S) health check path, e.g. `/healthz` |
| `edgecdnx-health-protocol` | `http` (default) or `https` |
| `edgecdnx-health-port` | health check port (defaults to 80/443 by protocol) |

Rules:

- Non-simple routing requires the standard
  `external-dns.alpha.kubernetes.io/set-identifier` annotation (it becomes the
  Location name) and works for `A`/`AAAA` records only. Violations are rejected
  with HTTP 400.
- Locations created by the webhook are labelled
  `app.kubernetes.io/managed-by: edgecdnx-external-dns-webhook` and
  `dns.edgecdnx.com/endpoint: <hostname>`; existing Locations without these
  labels are never adopted, modified, or deleted.
- Multiple clusters can publish the same hostname with distinct
  set-identifiers; they converge on one shared DNSEndpoint. Each cluster's
  ExternalDNS must run with a distinct `--txt-owner-id` **and** `--txt-prefix`.

## Protocol and limitations

- Supports ExternalDNS webhook media type `application/external.dns.webhook+json;version=1`.
- Implements `GET /`, `GET /records`, `POST /records`, `POST /adjustendpoints`,
  and `GET /healthz`.
- Webhook request and EdgeCDN-X API response bodies are limited to 32 MiB; the
  webhook limit matches ExternalDNS's default.
- DNS owner names use the API's record naming convention. Zone selection uses
  the most-specific matching Project Zone and DNS label boundaries.
- TTL `0` from ExternalDNS (no `external-dns.alpha.kubernetes.io/ttl`
  annotation) is stored as-is; resolvers will not cache such records.
- `setIdentifier` is supported together with the `edgecdnx-routing` annotations
  (non-Simple routing) and on TXT registry records, where it is persisted as an
  extra `edgecdnx-set-identifier=<id>` TXT string. It is rejected on plain
  Simple records.
- `labels` and other `providerSpecific` properties are currently not persisted
  by the API.

## Development

Requires Go 1.25 or newer:

```sh
make test
make vet
make build
```

The tests use an in-memory EdgeCDN-X API fixture and cover negotiation, endpoint
conversion, read-only service endpoints, and version-checked create/update/delete
flows.
