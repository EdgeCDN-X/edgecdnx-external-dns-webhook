# End-to-end test harness

Simulates the multi-cluster scenarios from
[docs/routing-policies-plan.md](../docs/routing-policies-plan.md): two kind
clusters (`fra`, `ams`) run ExternalDNS with the EdgeCDN-X webhook as a
sidecar and publish the **same hostname** with different set-identifiers
against a real EdgeCDN-X API.

## Prerequisites

- `kind`, `docker`, `kubectl`, `curl` (and `envsubst` or `python3`)
- An EdgeCDN-X Project with a zone covering the test hostname and a
  project-scoped API key

## Usage

```sh
cp .env.example .env   # fill in API credentials, hostname, scenario
make all               # create clusters, build + load image, deploy, ingress
make status            # pods, ingresses, external-dns + webhook logs
make verify            # list resulting Locations/profiles/DNS endpoints
make cleanup           # delete both kind clusters
```

Individual steps are available as `make create`, `make build`, `make deploy`,
and `make ingress`. Re-run `make ingress` after changing `E2E_ROUTING` or the
per-cluster variables in `.env`; re-run `make build deploy` after changing the
webhook code.

## What it does

- `create` — creates kind clusters `fra` and `ams`.
- `build` — builds the webhook image from this repository and loads it into
  both clusters (no registry needed).
- `deploy` — deploys ExternalDNS (`--source=ingress`, `--provider=webhook`)
  with the webhook sidecar into namespace `external-dns` of each cluster. Each
  cluster gets a distinct `--txt-owner-id`/`--txt-prefix` (`fra`/`fra-`,
  `ams`/`ams-`), which is required when sharing a hostname.
- `ingress` — creates one Ingress per cluster for `E2E_HOSTNAME` with the
  `webhook-edgecdnx-*` annotations for the selected `E2E_ROUTING` scenario
  (`wrr`, `geo`, `rr`, or `failover`). The record target comes from the
  `external-dns.kubernetes.io/target` annotation (`FRA_TARGET` /
  `AMS_TARGET`), so no ingress controller is needed.

Note: external-dns v0.22.0 changed the default annotation prefix from
`external-dns.alpha.kubernetes.io/` to `external-dns.kubernetes.io/`. The
harness uses the new prefix by default; set `E2E_ANNOTATION_PREFIX` in `.env`
when testing against an older external-dns image.

Expected result (e.g. `wrr`): two Locations (`kind-fra` weight 70, `kind-ams`
weight 30) with the health check profiles attached, and one shared `Weighted`
DNSEndpoint for the hostname — inspect with `make verify`.

The `.env` file holds the API key; it is gitignored — never commit it.
