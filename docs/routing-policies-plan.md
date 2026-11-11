# Plan: Routing-policy support (geo, wrr, rr, failover) via ExternalDNS annotations

Status: **Draft / not implemented**

## Goal

Extend the webhook beyond `Simple` DNS records. Users annotate an Ingress/Route and the
webhook provisions the full EdgeCDN-X routing stack for that hostname:

- a `DNSEndpoint` with the requested routing policy,
- a `Location` per `setIdentifier` (node IPs taken from the record targets, one default
  node group),
- an optional `HealthCheckProfile` (HTTP/HTTPS) attached to the node group.

## How annotations reach the webhook

ExternalDNS converts annotations prefixed with

```
external-dns.kubernetes.io/webhook-<name>: <value>
```

into `providerSpecific` properties named `webhook/<name>` on each endpoint it sends to
the webhook (`/adjustendpoints` and `POST /records`). The webhook therefore matches
`providerSpecific` entries named `webhook/edgecdnx-*`; it never sees the annotation
itself. `setIdentifier` comes from the standard
`external-dns.kubernetes.io/set-identifier` annotation and arrives in the
`setIdentifier` endpoint field.

> Prefix note: `external-dns.kubernetes.io/` is the default annotation prefix since
> ExternalDNS v0.22.0. Older releases use `external-dns.alpha.kubernetes.io/`;
> v0.22.0+ only reads the legacy prefix when started with
> `--enable-legacy-annotation-prefix`. The providerSpecific names seen by the webhook
> (`webhook/edgecdnx-*`) are identical with either prefix.

## Annotation reference

All names below are suffixes of `external-dns.kubernetes.io/webhook-`.

| Annotation suffix | Applies to | Value | Maps to |
|---|---|---|---|
| `edgecdnx-routing` | all | `simple` (default), `geo`, `wrr`, `rr`, `failover` | `DNSEndpoint.spec.routingPolicy`: `Simple`, `Geolocation`, `Weighted`, `RoundRobin`, `Failover` |
| `edgecdnx-weight` | `wrr` (required) | integer `0..1000` | `Location.spec.weight` |
| `edgecdnx-continents` | `geo` (optional) | comma-delimited continent codes, e.g. `EU,AF,SA` | `Location.spec.geoLookup.attributes["geoip/continent/code"].values` |
| `edgecdnx-countries` | `geo` (optional) | comma-delimited ISO 3166-1 alpha-2 codes, e.g. `SK,HU,NL` | `Location.spec.geoLookup.attributes["geoip/country/code"].values` |
| `edgecdnx-geo-default` | `geo` (optional) | `true` or `false` (default `false`) | `Location.spec.geoLookup.attributes["default"]` with weight `1` (catch-all candidate) |
| `edgecdnx-fallback` | `failover` (required for `primary`, optional for `secondary`) | location name (DNS-1123) | `Location.spec.fallbackLocations[0]` |
| `edgecdnx-failover-role` | `failover` (optional) | `primary` (default) or `secondary` | primary: `DNSEndpoint.spec.targets[0] = setIdentifier`; secondary: Location only |
| `edgecdnx-health-endpoint` | non-simple (optional) | request path, e.g. `/healthz` | `HealthCheckProbeSpec.http.path` |
| `edgecdnx-health-protocol` | with health-endpoint | `http` or `https` (default `http`) | `HealthCheckProbeSpec.http.protocol` |
| `edgecdnx-health-port` | with health-endpoint | `1..65535` (default `80`/`443` by protocol) | `HealthCheckProbeSpec.http.port` |

Example:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  annotations:
    external-dns.kubernetes.io/set-identifier: fra-edge-1
    external-dns.kubernetes.io/webhook-edgecdnx-routing: geo
    external-dns.kubernetes.io/webhook-edgecdnx-continents: EU
    external-dns.kubernetes.io/webhook-edgecdnx-countries: DE,AT,CH
    external-dns.kubernetes.io/webhook-edgecdnx-health-endpoint: /healthz
    external-dns.kubernetes.io/webhook-edgecdnx-health-protocol: https
    external-dns.kubernetes.io/webhook-edgecdnx-health-port: "443"
```

## Validation rules (reject with HTTP 400)

Validation happens when converting an incoming endpoint; failures are returned as
`permanentProviderError`, which `writeProviderError` already maps to `400`.

1. `edgecdnx-routing` must be one of `simple|geo|wrr|rr|failover` (case-insensitive).
   Missing/empty means `simple`.
2. If routing is not `simple`, `setIdentifier` **must** be set, non-empty, and a valid
   DNS-1123 subdomain (it becomes the Location name). Reject otherwise.
3. If routing is `simple`, `setIdentifier` must be empty (current behaviour preserved)
   and no other `edgecdnx-*` property may be present.
4. `wrr`: `edgecdnx-weight` required, integer in `0..1000`.
5. `geo`: `edgecdnx-continents` / `edgecdnx-countries` / `edgecdnx-geo-default` optional,
   but at least one of the three must be present (`geo-default: false` does not count);
   each continent/country entry is an uppercase 2-letter code after trimming;
   empty entries rejected; `edgecdnx-geo-default` must parse as a boolean.
6. `failover`: `edgecdnx-failover-role` must be `primary` (default) or `secondary`.
   For `primary`, `edgecdnx-fallback` is required (valid DNS-1123 subdomain); for
   `secondary` it is optional (used for fallback chains). `edgecdnx-failover-role`
   is rejected on any other policy.
7. Policy-specific properties on the wrong policy (e.g. `weight` with `geo`) are
   rejected to avoid silent misconfiguration.
8. `edgecdnx-health-protocol`/`edgecdnx-health-port` without `edgecdnx-health-endpoint`
   are rejected; path must start with `/`; port in `1..65535`.
9. Non-simple routing is only supported for `A` and `AAAA` records (targets must be IP
   addresses so they can become node IPs). Reject for `CNAME`, `TXT`, etc.

`/adjustendpoints` returns endpoints unchanged except for dropping unknown
`webhook/edgecdnx-*` properties is **not** done — unknown `edgecdnx-*` names are
rejected (fail fast), other `webhook/*` names are ignored.

## Resource model

For an endpoint `hostname` + `recordType` + `setIdentifier` with non-simple routing:

### Location (name = `setIdentifier`)

```yaml
name: <setIdentifier>
labels:
  dns.edgecdnx.com/endpoint: <hostname>            # matched by routeSelector
  app.kubernetes.io/managed-by: edgecdnx-external-dns-webhook
nodeGroups:
  - name: default
    flavor: ""
    healthCheck: { name: <profile-name> }           # only when health-endpoint set
    nodes:
      - name: default
        ipv4: <A targets>        # from A record targets
        ipv6: <AAAA targets>     # from AAAA record targets
weight: <edgecdnx-weight>                           # wrr only
geoLookup:                                          # geo only
  attributes:
    geoip/continent/code: { weight: 1000, values: [{ value: EU }] }
    geoip/country/code:   { weight: 100,  values: [{ value: DE }, ...] }
fallbackLocations: [<edgecdnx-fallback>]            # failover only
```

Notes:

- The CoreDNS plugin merges Location + node-group labels and matches them against
  `DNSEndpoint.spec.routeSelector`, so the `dns.edgecdnx.com/endpoint` label on the
  Location is sufficient.
- Attribute weights follow the documented convention (continent `1000`, country `100`)
  so country matches outrank continent matches.
- `NodeSpec` holds a single `ipv4`/`ipv6` each. First target of each family is used;
  additional targets create additional nodes (`default-2`, …) in the same node group.
- If the hostname exceeds 63 chars (label value limit), use
  `dns.edgecdnx.com/endpoint: sha256-<first-16-hex>` derived from the normalized
  hostname; the same derivation is used on the `DNSEndpoint.routeSelector`.

### HealthCheckProfile (only when `edgecdnx-health-endpoint` is set)

```yaml
name: <setIdentifier>                  # 1:1 with the Location
probes:
  - name: http
    type: HTTP
    http:
      protocol: <health-protocol>      # http|https
      port: <health-port>
      path: <health-endpoint>
      host: <hostname>                 # Host header = record hostname
```

### DNSEndpoint (one per `hostname` + `recordType`, shared across setIdentifiers)

```yaml
dnsName: <hostname>
recordType: A|AAAA
recordTTL: <ttl>
routingPolicy: Geolocation|Weighted|RoundRobin|Failover
routeSelector:                          # geo / wrr / rr
  matchLabels:
    dns.edgecdnx.com/endpoint: <hostname>
targets: [<setIdentifier>]              # failover only: primary location name
```

Per the CoreDNS plugin, `Geolocation`/`Weighted`/`RoundRobin` resolve candidate
Locations through `routeSelector`; `Failover` uses `targets[0]` as the primary Location
and follows `fallbackLocations` from there.

Failover specifics (multi-cluster safe):

- The `primary` endpoint upserts `targets: [<its setIdentifier>]`. If the DNSEndpoint
  already has a different primary, that is a conflict → `400` (exactly one primary per
  hostname + record type).
- A `secondary` endpoint only maintains its Location (so the primary's
  `fallbackLocations` can reference it). It creates the DNSEndpoint with empty
  `targets` if it syncs before the primary (the plugin falls through until the primary
  registers), and never overwrites a non-empty `targets`.

## Provider flow changes

### `records` (GET)

Today the provider lists DNS records and skips/errors on non-simple ones. New
behaviour:

- `Simple` records: unchanged.
- Non-simple records: for each record, list the project's Locations that carry the
  `app.kubernetes.io/managed-by: edgecdnx-external-dns-webhook` label and whose
  `dns.edgecdnx.com/endpoint` label matches the record's hostname. Emit **one endpoint
  per Location** with:
  - `setIdentifier` = location name,
  - `targets` = node IPs of the matching family (ipv4 for A, ipv6 for AAAA),
  - `providerSpecific` reconstructed from the Location/profile state
    (`webhook/edgecdnx-routing`, `webhook/edgecdnx-weight`, …).

  Reconstruction must be lossless w.r.t. the annotation grammar, otherwise ExternalDNS
  will see a permanent diff and loop. This is the main correctness risk — covered by
  round-trip tests (annotations → resources → records → equality).
- Non-simple records whose Locations lack the managed-by label are skipped (treated as
  externally managed; ExternalDNS must not touch them).

### `apply` (POST /records)

Replace the current hard rejections (`setIdentifier is not supported`, `cannot
update/delete DNS endpoint with routing policy`) with policy-aware handling:

**Create** (per endpoint, non-simple):

1. Validate (section above). Derive desired Location, optional HealthCheckProfile,
   and DNSEndpoint.
2. Upsert HealthCheckProfile (create; on conflict compare & update).
3. Upsert Location. If a Location with that name exists **without** the managed-by
   label → `400` (never adopt foreign Locations).
4. Upsert DNSEndpoint:
   - not found → create with routing policy + routeSelector (or targets for failover);
   - found with same policy + managed-by semantics → update TTL/selector if needed;
   - found with different policy or `Simple` → `400` (conflicting definitions for the
     same hostname).

   A+AAAA for the same hostname/setIdentifier yield two DNSEndpoints but share one
   Location; the Location upsert merges `ipv4`/`ipv6` instead of overwriting.

**Update** (`updateOld`/`updateNew`): identity = `dnsName + recordType +
setIdentifier` (already implemented in `sameEndpointIdentity`). Same-identity changes
diff the derived resources and patch Location/profile/DNSEndpoint (optimistic
concurrency via `resourceVersion`, reusing the existing retry loop). Identity changes
(e.g. new setIdentifier) are create-new + delete-old, which the current `apply` already
does.

**Delete**:

1. Delete the Location named `setIdentifier` (only if managed-by label present) —
   but only remove the record-type's IP family first; delete the Location when it no
   longer has any node IPs (A and AAAA are deleted independently by ExternalDNS).
2. Delete the associated HealthCheckProfile when the Location is deleted.
3. If no other managed Location matches the `dns.edgecdnx.com/endpoint` label for this
   hostname, delete the DNSEndpoint. Otherwise leave it (other setIdentifiers remain).
4. For failover, if the deleted Location was `targets[0]` of the DNSEndpoint (the
   primary), delete the DNSEndpoint as well (the API cannot patch `targets` to empty);
   remaining secondary clusters detect the missing record on their next sync and
   recreate it with empty `targets`, converging once a new primary registers.

All API errors keep the existing translation: permanent/validation issues → `400`,
upstream 5xx → `502`.

### Ordering & atomicity

Creation order: profile → location → dnsendpoint. Deletion order: dnsendpoint (or
target removal) → location → profile. There is no cross-resource transaction in the
API; a failure mid-sequence returns an error so ExternalDNS retries — all upserts must
therefore be idempotent.

### TXT registry records

ExternalDNS's TXT registry copies the **setIdentifier and providerSpecific
properties** of each source endpoint onto the ownership TXT record it generates, and
ownership matching requires the setIdentifier to round-trip through the provider.
Handling:

- TXT records are always stored as `Simple` records; `edgecdnx-*` properties on them
  are ignored (never validated as routing annotations).
- The DNSEndpoint API has no setIdentifier field, so a TXT record's setIdentifier is
  persisted as an extra TXT target `edgecdnx-set-identifier=<id>`, appended on write
  and stripped (and surfaced as `setIdentifier`) when listing records. The marker is
  visible in actual DNS responses for the ownership record, which is harmless.
- The API derives the record resource name from `dnsName + recordType`, so two
  clusters' registry records for the same hostname must not share a TXT name:
  **each cluster must run ExternalDNS with a distinct `--txt-prefix` (and
  `--txt-owner-id`)**.

## Multi-cluster scenario walkthroughs

Common setup for all scenarios below: two clusters run their own ExternalDNS +
webhook instance, both pointing at the **same EdgeCDN-X project**. Each cluster
publishes the same hostname `app.example.com` from its own ingress controller:

| Cluster | `set-identifier` | LB IPv4 |
|---|---|---|
| Frankfurt | `k8s-fra` | `203.0.113.10` |
| Amsterdam | `k8s-ams` | `198.51.100.20` |

The `setIdentifier` is what keeps the two clusters from fighting over each other's
state: each cluster owns exactly its own Location (`k8s-fra` / `k8s-ams`), while the
shared DNSEndpoint (`app.example.com-a`) is converged by both via idempotent upserts.
Each cluster's ExternalDNS must use a distinct `--txt-owner-id` **and**
`--txt-prefix` (see the TXT registry section above).

### Scenario 1 — `wrr` (70/30 split)

Frankfurt Ingress:

```yaml
metadata:
  annotations:
    external-dns.kubernetes.io/set-identifier: k8s-fra
    external-dns.kubernetes.io/webhook-edgecdnx-routing: wrr
    external-dns.kubernetes.io/webhook-edgecdnx-weight: "70"
    external-dns.kubernetes.io/webhook-edgecdnx-health-endpoint: /healthz
    external-dns.kubernetes.io/webhook-edgecdnx-health-protocol: https
    external-dns.kubernetes.io/webhook-edgecdnx-health-port: "443"
```

Amsterdam Ingress: same, with `set-identifier: k8s-ams` and `edgecdnx-weight: "30"`.

Endpoint the Frankfurt ExternalDNS sends to its webhook (`POST /records`, create):

```json
{
  "dnsName": "app.example.com",
  "recordType": "A",
  "setIdentifier": "k8s-fra",
  "recordTTL": 60,
  "targets": ["203.0.113.10"],
  "providerSpecific": [
    {"name": "webhook/edgecdnx-routing", "value": "wrr"},
    {"name": "webhook/edgecdnx-weight", "value": "70"},
    {"name": "webhook/edgecdnx-health-endpoint", "value": "/healthz"},
    {"name": "webhook/edgecdnx-health-protocol", "value": "https"},
    {"name": "webhook/edgecdnx-health-port", "value": "443"}
  ]
}
```

Resulting EdgeCDN-X resources after **both** clusters have synced:

```yaml
# HealthCheckProfile (one per cluster; fra shown, ams identical with its name)
name: k8s-fra
probes:
  - name: http
    type: HTTP
    http: { protocol: https, port: 443, path: /healthz, host: app.example.com }
---
# Location owned by Frankfurt's webhook
name: k8s-fra
labels:
  dns.edgecdnx.com/endpoint: app.example.com
  app.kubernetes.io/managed-by: edgecdnx-external-dns-webhook
weight: 70
nodeGroups:
  - name: default
    flavor: ""
    healthCheck: { name: k8s-fra }
    nodes: [{ name: default, ipv4: 203.0.113.10 }]
---
# Location owned by Amsterdam's webhook
name: k8s-ams
labels:
  dns.edgecdnx.com/endpoint: app.example.com
  app.kubernetes.io/managed-by: edgecdnx-external-dns-webhook
weight: 30
nodeGroups:
  - name: default
    flavor: ""
    healthCheck: { name: k8s-ams }
    nodes: [{ name: default, ipv4: 198.51.100.20 }]
---
# Shared DNSEndpoint — first cluster creates it, the second finds it identical (no-op)
name: app.example.com-a
dnsName: app.example.com
recordType: A
recordTTL: 60
routingPolicy: Weighted
routeSelector:
  matchLabels:
    dns.edgecdnx.com/endpoint: app.example.com
```

Resolution: CoreDNS selects `k8s-fra` ~70% / `k8s-ams` ~30% of queries (smooth
weighted RR over healthy locations) and answers with that location's node IP.

### Scenario 2 — `geo` (EU → Frankfurt, Americas → Amsterdam)

Frankfurt:

```yaml
external-dns.kubernetes.io/set-identifier: k8s-fra
external-dns.kubernetes.io/webhook-edgecdnx-routing: geo
external-dns.kubernetes.io/webhook-edgecdnx-continents: EU
external-dns.kubernetes.io/webhook-edgecdnx-countries: GB,CH
```

Amsterdam:

```yaml
external-dns.kubernetes.io/set-identifier: k8s-ams
external-dns.kubernetes.io/webhook-edgecdnx-routing: geo
external-dns.kubernetes.io/webhook-edgecdnx-continents: "NA,SA"
```

Resulting Locations (DNSEndpoint as in scenario 1 but `routingPolicy: Geolocation`):

```yaml
name: k8s-fra
labels: { dns.edgecdnx.com/endpoint: app.example.com, app.kubernetes.io/managed-by: edgecdnx-external-dns-webhook }
geoLookup:
  attributes:
    geoip/continent/code: { weight: 1000, values: [{ value: EU }] }
    geoip/country/code:   { weight: 100,  values: [{ value: GB }, { value: CH }] }
nodeGroups:
  - name: default
    flavor: ""
    nodes: [{ name: default, ipv4: 203.0.113.10 }]
---
name: k8s-ams
labels: { dns.edgecdnx.com/endpoint: app.example.com, app.kubernetes.io/managed-by: edgecdnx-external-dns-webhook }
geoLookup:
  attributes:
    geoip/continent/code: { weight: 1000, values: [{ value: NA }, { value: SA }] }
nodeGroups:
  - name: default
    flavor: ""
    nodes: [{ name: default, ipv4: 198.51.100.20 }]
```

Resolution: a resolver in Germany scores `k8s-fra` (continent EU = 1000); one in
Brazil scores `k8s-ams` (continent SA = 1000); one in the UK scores `k8s-fra`
higher (EU 1000 + GB 100). Resolvers matching neither get no geo candidate and fall
through to the next plugin.

### Scenario 3 — `rr` (plain round robin)

Both clusters only set:

```yaml
external-dns.kubernetes.io/set-identifier: k8s-fra   # / k8s-ams
external-dns.kubernetes.io/webhook-edgecdnx-routing: rr
```

Locations carry just the two labels, the default node group, and the node IP — no
weight, no geoLookup. The shared DNSEndpoint has `routingPolicy: RoundRobin` with the
same `routeSelector`. CoreDNS alternates between `k8s-fra` and `k8s-ams`.

### Scenario 4 — `failover` (Frankfurt primary, Amsterdam standby)

Frankfurt (primary):

```yaml
external-dns.kubernetes.io/set-identifier: k8s-fra
external-dns.kubernetes.io/webhook-edgecdnx-routing: failover
external-dns.kubernetes.io/webhook-edgecdnx-fallback: k8s-ams
external-dns.kubernetes.io/webhook-edgecdnx-health-endpoint: /healthz
```

Amsterdam (secondary):

```yaml
external-dns.kubernetes.io/set-identifier: k8s-ams
external-dns.kubernetes.io/webhook-edgecdnx-routing: failover
external-dns.kubernetes.io/webhook-edgecdnx-failover-role: secondary
external-dns.kubernetes.io/webhook-edgecdnx-health-endpoint: /healthz
```

Resulting resources:

```yaml
# Location: primary, owned by Frankfurt
name: k8s-fra
labels: { dns.edgecdnx.com/endpoint: app.example.com, app.kubernetes.io/managed-by: edgecdnx-external-dns-webhook }
fallbackLocations: [k8s-ams]
nodeGroups:
  - name: default
    flavor: ""
    healthCheck: { name: k8s-fra }
    nodes: [{ name: default, ipv4: 203.0.113.10 }]
---
# Location: standby, owned by Amsterdam (no fallbackLocations — end of chain)
name: k8s-ams
labels: { dns.edgecdnx.com/endpoint: app.example.com, app.kubernetes.io/managed-by: edgecdnx-external-dns-webhook }
nodeGroups:
  - name: default
    flavor: ""
    healthCheck: { name: k8s-ams }
    nodes: [{ name: default, ipv4: 198.51.100.20 }]
---
# Shared DNSEndpoint — only the primary writes targets
name: app.example.com-a
dnsName: app.example.com
recordType: A
recordTTL: 60
routingPolicy: Failover
targets: [k8s-fra]
```

Sync-order independence: if Amsterdam syncs first, it creates the DNSEndpoint with
`routingPolicy: Failover` and empty `targets` (the plugin falls through until a primary
exists); Frankfurt then sets `targets: [k8s-fra]`. A second cluster claiming `primary`
is rejected with 400. Resolution: all queries go to `k8s-fra`; when its health check
fails, the plugin walks `fallbackLocations` to `k8s-ams`.

### Dual-stack note (applies to all scenarios)

If a cluster also publishes an AAAA record (same hostname + setIdentifier), ExternalDNS
sends a second endpoint (`recordType: AAAA`, IPv6 targets). The webhook creates a
**second DNSEndpoint** (`app.example.com-aaaa`) but **merges into the same Location**:

```yaml
nodes: [{ name: default, ipv4: 203.0.113.10, ipv6: "2001:db8::10" }]
```

Deleting only the A record removes `ipv4` from the nodes and deletes
`app.example.com-a`; the Location survives while `ipv6` remains. The Location and its
HealthCheckProfile are deleted when the last IP family is removed; the shared
DNSEndpoint is deleted when the last matching managed Location (for its IP family)
disappears.

## Code structure

| File | Change |
|---|---|
| `annotations.go` (new) | Parse + validate `webhook/edgecdnx-*` providerSpecific props into a typed `routingConfig`; serialize back for `records` reconstruction; canonicalize endpoints for `/adjustendpoints`. |
| `resources.go` (new) | Pure functions `routingConfig` + endpoint → desired Location / HealthCheckProfile / routeSelector; diff helpers for updates; reconstruction of `routingConfig` from API resources. |
| `managed.go` (new) | Managed create/update/delete flows (profile → location → dnsendpoint upserts, family merge, GC) and `records` reconstruction per Location. |
| `provider.go` | Dispatch simple vs. managed endpoints in `records` and `apply`; persist TXT registry setIdentifiers via target marker. |
| `server.go` | `/adjustendpoints` validates and canonicalizes routing annotations (400 on invalid). |
| `README.md` | Document the annotations with examples. |
| SDK (`edgecdnx-sdk-go`) | Verify `LocationsService`/`HealthCheckProfilesService` cover label-based list filtering; if the API lacks label filters, filter client-side after `List`. |
| API (`edgecdnx-api`) | Verify the locations module accepts `labels`, `nodeGroups[].healthCheck`, `geoLookup`, `weight`, `fallbackLocations` on create/patch. |

## Testing plan

- Unit: annotation parsing/validation matrix (every rule in the validation section,
  including 400 on missing setIdentifier).
- Unit: resource derivation — golden structs per policy (geo with/without countries,
  wrr, rr, failover, with/without health endpoint).
- Round-trip: endpoint → resources → reconstructed endpoint is identical (no
  ExternalDNS diff loops).
- Provider tests (httptest-mocked API, as in `provider_test.go`): create/update/delete
  sequences per policy, A+AAAA location sharing, multi-setIdentifier shared
  DNSEndpoint, refusal to adopt unmanaged Locations, conflict retries.
- Server tests: `POST /records` returns 400 for each validation failure.

## Open questions / follow-ups

1. **Failover primary ambiguity**: resolved via `edgecdnx-failover-role`
   (`primary`/`secondary`, see annotation reference and the failover walkthrough).
   Exactly one primary per hostname + record type; a second primary is rejected
   with 400.
2. **Weight updates vs. GeoLookup weight**: `wrr` uses `Location.spec.weight`;
   `Location.spec.geoLookup.weight` (tie-breaking among equal geo scores) is not
   exposed via annotations for now.
3. **TTL semantics**: non-simple responses are synthesized by the CoreDNS plugin;
   confirm `recordTTL` on the DNSEndpoint is honored there.
4. **Health check for TCP/ASSUME**: out of scope; only HTTP/HTTPS probes are exposed.
5. **Label filter support in the API**: if `GET /project/{id}/locations` cannot filter
   by label, `records` does an O(locations) client-side scan per project — acceptable
   at current scale, revisit if needed.
