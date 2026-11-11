#!/usr/bin/env bash
# End-to-end test harness: two kind clusters (fra, ams) running ExternalDNS
# with the EdgeCDN-X webhook as a sidecar, publishing the same hostname with
# different set-identifiers. See e2e/.env.example for configuration.
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$E2E_DIR")"
CLUSTERS=(fra ams)

usage() {
  cat <<'EOF'
Usage: e2e.sh <command>

Commands:
  create    Create the kind clusters (fra, ams)
  build     Build the webhook image and load it into both clusters
  deploy    Deploy ExternalDNS + webhook sidecar into both clusters
  ingress   Create/update the e2e Ingress (same hostname) in both clusters
  status    Show pods, ingresses, and recent logs from both clusters
  verify    List the resulting Locations and DNS endpoints from the API
  cleanup   Delete both kind clusters
  all       create + build + deploy + ingress
EOF
  exit 1
}

load_env() {
  if [[ ! -f "$E2E_DIR/.env" ]]; then
    echo "error: $E2E_DIR/.env not found; copy .env.example and fill it in" >&2
    exit 1
  fi
  set -a
  # shellcheck disable=SC1091
  source "$E2E_DIR/.env"
  set +a
  : "${WEBHOOK_IMAGE:=edgecdnx-external-dns-webhook:e2e}"
  : "${EXTERNAL_DNS_IMAGE:=registry.k8s.io/external-dns/external-dns:v0.24.0}"
  : "${LOG_LEVEL:=debug}"
  : "${E2E_ROUTING:=wrr}"
  # Default prefix since external-dns v0.22.0. For older releases set
  # E2E_ANNOTATION_PREFIX=external-dns.alpha.kubernetes.io in .env.
  : "${E2E_ANNOTATION_PREFIX:=external-dns.kubernetes.io}"
}

require_env() {
  local missing=0
  for name in "$@"; do
    if [[ -z "${!name:-}" ]]; then
      echo "error: $name must be set in e2e/.env" >&2
      missing=1
    fi
  done
  [[ "$missing" -eq 0 ]]
}

require_tools() {
  for tool in "$@"; do
    command -v "$tool" >/dev/null || { echo "error: $tool is required" >&2; exit 1; }
  done
}

# render substitutes ${VAR} references in stdin from the environment.
render() {
  if command -v envsubst >/dev/null; then
    envsubst
  else
    python3 -c 'import os, sys; sys.stdout.write(os.path.expandvars(sys.stdin.read()))'
  fi
}

ctx() { echo "kind-$1"; }

cmd_create() {
  require_tools kind
  for cluster in "${CLUSTERS[@]}"; do
    if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
      echo "kind cluster $cluster already exists"
    else
      kind create cluster --name "$cluster"
    fi
  done
}

cmd_build() {
  load_env
  require_tools kind docker
  docker build -t "$WEBHOOK_IMAGE" "$REPO_DIR"
  for cluster in "${CLUSTERS[@]}"; do
    kind load docker-image "$WEBHOOK_IMAGE" --name "$cluster"
  done
}

cmd_deploy() {
  load_env
  require_tools kubectl
  require_env EDGECDNX_API_URL EDGECDNX_PROJECT_ID EDGECDNX_API_KEY
  for cluster in "${CLUSTERS[@]}"; do
    local context
    context="$(ctx "$cluster")"
    echo "==> deploying to $context"
    # Each cluster gets a distinct TXT owner and prefix (required for the
    # shared hostname; see docs/routing-policies-plan.md, TXT registry).
    TXT_OWNER="$cluster" TXT_PREFIX="$cluster-" CLUSTER="$cluster" \
      render <"$E2E_DIR/manifests/external-dns.yaml.tmpl" |
      kubectl --context "$context" apply -f -
    kubectl --context "$context" --namespace external-dns \
      create secret generic edgecdnx-api-key \
      --from-literal=apiKey="$EDGECDNX_API_KEY" \
      --dry-run=client -o yaml | kubectl --context "$context" apply -f -
    kubectl --context "$context" --namespace external-dns \
      rollout restart deployment external-dns >/dev/null 2>&1 || true
    kubectl --context "$context" --namespace external-dns \
      rollout status deployment external-dns --timeout=120s
  done
}

# routing_annotations prints the policy-specific annotation lines (4-space
# indented) for one cluster, based on E2E_ROUTING and the FRA_/AMS_ variables.
routing_annotations() {
  local prefix="$1"
  local weight continents countries role fallback
  weight="$(var "$prefix" WEIGHT)"
  continents="$(var "$prefix" CONTINENTS)"
  countries="$(var "$prefix" COUNTRIES)"
  role="$(var "$prefix" FAILOVER_ROLE)"
  fallback="$(var "$prefix" FALLBACK)"
  case "$E2E_ROUTING" in
    wrr)
      [[ -n "$weight" ]] || { echo "error: ${prefix}_WEIGHT is required for wrr" >&2; exit 1; }
      printf '    %s/webhook-edgecdnx-weight: "%s"\n' "$E2E_ANNOTATION_PREFIX" "$weight"
      ;;
    geo)
      [[ -n "$continents$countries" ]] || { echo "error: ${prefix}_CONTINENTS or ${prefix}_COUNTRIES is required for geo" >&2; exit 1; }
      [[ -z "$continents" ]] || printf '    %s/webhook-edgecdnx-continents: "%s"\n' "$E2E_ANNOTATION_PREFIX" "$continents"
      [[ -z "$countries" ]] || printf '    %s/webhook-edgecdnx-countries: "%s"\n' "$E2E_ANNOTATION_PREFIX" "$countries"
      ;;
    failover)
      [[ -z "$role" ]] || printf '    %s/webhook-edgecdnx-failover-role: "%s"\n' "$E2E_ANNOTATION_PREFIX" "$role"
      [[ -z "$fallback" ]] || printf '    %s/webhook-edgecdnx-fallback: "%s"\n' "$E2E_ANNOTATION_PREFIX" "$fallback"
      ;;
    rr) ;;
    *)
      echo "error: unsupported E2E_ROUTING=$E2E_ROUTING (use wrr|geo|rr|failover)" >&2
      exit 1
      ;;
  esac
}

health_annotations() {
  [[ -n "${E2E_HEALTH_ENDPOINT:-}" ]] || return 0
  printf '    %s/webhook-edgecdnx-health-endpoint: "%s"\n' "$E2E_ANNOTATION_PREFIX" "$E2E_HEALTH_ENDPOINT"
  [[ -z "${E2E_HEALTH_PROTOCOL:-}" ]] || printf '    %s/webhook-edgecdnx-health-protocol: "%s"\n' "$E2E_ANNOTATION_PREFIX" "$E2E_HEALTH_PROTOCOL"
  [[ -z "${E2E_HEALTH_PORT:-}" ]] || printf '    %s/webhook-edgecdnx-health-port: "%s"\n' "$E2E_ANNOTATION_PREFIX" "$E2E_HEALTH_PORT"
}

# var FRA WEIGHT -> value of $FRA_WEIGHT (empty when unset)
var() {
  local name="$1_$2"
  echo "${!name:-}"
}

cmd_ingress() {
  load_env
  require_tools kubectl
  require_env E2E_HOSTNAME E2E_ROUTING
  for cluster in "${CLUSTERS[@]}"; do
    local prefix context set_identifier target
    prefix="$(echo "$cluster" | tr '[:lower:]' '[:upper:]')"
    context="$(ctx "$cluster")"
    set_identifier="$(var "$prefix" SET_IDENTIFIER)"
    target="$(var "$prefix" TARGET)"
    [[ -n "$set_identifier" && -n "$target" ]] ||
      { echo "error: ${prefix}_SET_IDENTIFIER and ${prefix}_TARGET must be set" >&2; exit 1; }
    echo "==> creating ingress in $context (set-identifier=$set_identifier)"
    {
      cat <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: e2e-app
  namespace: default
  annotations:
    $E2E_ANNOTATION_PREFIX/target: "$target"
    $E2E_ANNOTATION_PREFIX/set-identifier: "$set_identifier"
    $E2E_ANNOTATION_PREFIX/ttl: "60"
    $E2E_ANNOTATION_PREFIX/webhook-edgecdnx-routing: "$E2E_ROUTING"
EOF
      routing_annotations "$prefix"
      health_annotations
      cat <<EOF
spec:
  rules:
    - host: $E2E_HOSTNAME
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: e2e-backend
                port:
                  number: 80
EOF
    } | kubectl --context "$context" apply -f -
  done
}

cmd_status() {
  load_env
  require_tools kubectl
  for cluster in "${CLUSTERS[@]}"; do
    local context
    context="$(ctx "$cluster")"
    echo "===================== $context ====================="
    kubectl --context "$context" --namespace external-dns get pods
    kubectl --context "$context" get ingress --all-namespaces
    echo "--- external-dns logs ($context) ---"
    kubectl --context "$context" --namespace external-dns logs deployment/external-dns \
      --container external-dns --tail=20 || true
    echo "--- webhook logs ($context) ---"
    kubectl --context "$context" --namespace external-dns logs deployment/external-dns \
      --container edgecdnx-webhook --tail=20 || true
  done
}

cmd_verify() {
  load_env
  require_tools curl
  require_env EDGECDNX_API_URL EDGECDNX_PROJECT_ID EDGECDNX_API_KEY
  echo "==> locations in project $EDGECDNX_PROJECT_ID"
  curl -sfS -H "Authorization: ApiKey $EDGECDNX_API_KEY" \
    "$EDGECDNX_API_URL/project/$EDGECDNX_PROJECT_ID/locations" | pretty_json
  echo
  echo "==> health check profiles in project $EDGECDNX_PROJECT_ID"
  curl -sfS -H "Authorization: ApiKey $EDGECDNX_API_KEY" \
    "$EDGECDNX_API_URL/project/$EDGECDNX_PROJECT_ID/healthcheckprofiles" | pretty_json
  if [[ -n "${E2E_ZONE:-}" ]]; then
    echo
    echo "==> DNS endpoints in zone $E2E_ZONE"
    curl -sfS -H "Authorization: ApiKey $EDGECDNX_API_KEY" \
      "$EDGECDNX_API_URL/project/$EDGECDNX_PROJECT_ID/zones/$E2E_ZONE/dns-endpoints" | pretty_json
  fi
}

pretty_json() {
  if command -v jq >/dev/null; then
    jq .
  elif command -v python3 >/dev/null; then
    python3 -m json.tool
  else
    cat
  fi
}

cmd_cleanup() {
  require_tools kind
  for cluster in "${CLUSTERS[@]}"; do
    if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
      kind delete cluster --name "$cluster"
    fi
  done
}

case "${1:-}" in
  create) cmd_create ;;
  build) cmd_build ;;
  deploy) cmd_deploy ;;
  ingress) cmd_ingress ;;
  status) cmd_status ;;
  verify) cmd_verify ;;
  cleanup) cmd_cleanup ;;
  all)
    cmd_create
    cmd_build
    cmd_deploy
    cmd_ingress
    ;;
  *) usage ;;
esac
