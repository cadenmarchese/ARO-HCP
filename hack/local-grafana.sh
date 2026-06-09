#!/usr/bin/env bash
set -euo pipefail

GRAFANA_VERSION="${GRAFANA_VERSION:-11.6.0}"
GRAFANA_PORT="${GRAFANA_PORT:-3000}"
CONTAINER_NAME="${CONTAINER_NAME:-aro-hcp-grafana}"
DEPLOY_ENV="${DEPLOY_ENV:-pers}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

usage() {
    cat <<EOF
Usage: $(basename "$0") [--start | --stop | --restart | --status]

Starts a local Grafana instance connected to your dev environment's
Azure Monitor Workspace Prometheus endpoints.

Prerequisites:
  - docker (or podman aliased to docker)
  - az CLI, logged into the dev subscription (az login)
  - templatize built (run 'make install-tools' from the repo root)

Environment variables:
  DEPLOY_ENV             Target environment (default: pers)
  GRAFANA_PORT           Local port (default: 3000)
  GRAFANA_VERSION        Grafana image tag (default: 11.6.0)
  CONTAINER_NAME         Docker container name (default: aro-hcp-grafana)

Commands:
  --start     Start the local Grafana (default if no flag given)
  --stop      Stop and remove the container
  --restart   Stop then start
  --status    Show container status
EOF
}

log() { echo "==> $*"; }

resolve_config() {
    local templatize="${REPO_ROOT}/tooling/templatize/templatize"
    if [[ ! -x "$templatize" ]]; then
        echo "ERROR: templatize binary not found at ${templatize}"
        echo "       Run 'make install-tools' from the repo root first."
        exit 1
    fi

    local tmpl
    tmpl="$(mktemp)"
    cat > "$tmpl" <<'TMPL'
REGION_RG={{ .regionRG }}
SVC_WORKSPACE_NAME={{ .monitoring.svcWorkspaceName }}
HCP_WORKSPACE_NAME={{ .monitoring.hcpWorkspaceName }}
TMPL

    local rendered
    rendered="$(mktemp)"
    "${templatize}" generate \
        --config-file "${REPO_ROOT}/config/config.yaml" \
        --dev-settings-file "${REPO_ROOT}/tooling/templatize/settings.yaml" \
        --dev-environment "${DEPLOY_ENV}" \
        --input "$tmpl" \
        --output "$rendered" 2>/dev/null

    # shellcheck disable=SC1090
    source "$rendered"
    rm -f "$tmpl" "$rendered"

    log "Resolved config for DEPLOY_ENV=${DEPLOY_ENV}:"
    log "  REGION_RG=${REGION_RG}"
    log "  SVC_WORKSPACE_NAME=${SVC_WORKSPACE_NAME}"
    log "  HCP_WORKSPACE_NAME=${HCP_WORKSPACE_NAME}"
}

get_prometheus_endpoint() {
    local name="$1"
    local rg="$2"
    az monitor account show \
        --name "$name" \
        --resource-group "$rg" \
        --query "metrics.prometheusQueryEndpoint" -o tsv 2>/dev/null
}

get_access_token() {
    az account get-access-token \
        --resource "https://prometheus.monitor.azure.com" \
        --query "accessToken" -o tsv 2>/dev/null
}

stop_grafana() {
    if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        log "Stopping and removing ${CONTAINER_NAME}..."
        docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1
        log "Stopped."
    else
        log "Container ${CONTAINER_NAME} is not running."
    fi
}

status_grafana() {
    if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        echo "Container ${CONTAINER_NAME} is running."
        echo "  URL: http://localhost:${GRAFANA_PORT}"
        docker ps --filter "name=${CONTAINER_NAME}" --format "  Status: {{.Status}}"
    elif docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        echo "Container ${CONTAINER_NAME} exists but is not running."
        docker ps -a --filter "name=${CONTAINER_NAME}" --format "  Status: {{.Status}}"
    else
        echo "Container ${CONTAINER_NAME} does not exist."
    fi
}

start_grafana() {
    if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        log "Container ${CONTAINER_NAME} is already running at http://localhost:${GRAFANA_PORT}"
        return 0
    fi

    # Remove stopped container if it exists
    docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true

    resolve_config

    log "Looking up AMW Prometheus query endpoints..."
    local svc_endpoint hcp_endpoint
    svc_endpoint="$(get_prometheus_endpoint "$SVC_WORKSPACE_NAME" "$REGION_RG")"
    hcp_endpoint="$(get_prometheus_endpoint "$HCP_WORKSPACE_NAME" "$REGION_RG")"

    if [[ -z "$svc_endpoint" ]]; then
        echo "ERROR: Could not resolve SVC AMW endpoint for ${SVC_WORKSPACE_NAME} in ${REGION_RG}."
        echo "       Are you logged into the correct Azure subscription? (az login)"
        exit 1
    fi
    if [[ -z "$hcp_endpoint" ]]; then
        echo "ERROR: Could not resolve HCP AMW endpoint for ${HCP_WORKSPACE_NAME} in ${REGION_RG}."
        echo "       Are you logged into the correct Azure subscription? (az login)"
        exit 1
    fi

    log "SVC endpoint: ${svc_endpoint}"
    log "HCP endpoint: ${hcp_endpoint}"

    log "Fetching Azure access token for prometheus.monitor.azure.com..."
    local token
    token="$(get_access_token)"
    if [[ -z "$token" ]]; then
        echo "ERROR: Could not get access token. Run 'az login' first."
        exit 1
    fi

    # Prepare provisioning directories
    local tmpdir
    tmpdir="$(mktemp -d)"
    trap 'rm -rf "${tmpdir}"' EXIT

    mkdir -p "${tmpdir}/provisioning/dashboards"
    mkdir -p "${tmpdir}/provisioning/datasources"

    # Generate dashboard provisioning from observability/observability.yaml so
    # that adding/removing dashboard folders there automatically takes effect.
    local obs_config="${REPO_ROOT}/observability/observability.yaml"
    if [[ ! -f "$obs_config" ]]; then
        echo "ERROR: Cannot find ${obs_config}"
        exit 1
    fi

    {
        echo "apiVersion: 1"
        echo "providers:"
        # Parse dashboardFolders entries: each has a name and path (relative to observability/).
        # Convert the path from ./grafana-dashboards/X to /var/lib/grafana/dashboards/X
        # (the container mount maps observability/grafana-dashboards -> /var/lib/grafana/dashboards).
        local in_folders=false name="" path=""
        while IFS= read -r line; do
            if [[ "$line" =~ ^[[:space:]]*dashboardFolders: ]]; then
                in_folders=true
                continue
            fi
            if $in_folders; then
                # Stop at next top-level key
                if [[ "$line" =~ ^[a-zA-Z] ]]; then
                    break
                fi
                if [[ "$line" =~ ^[[:space:]]*-[[:space:]]*name:[[:space:]]*(.*) ]]; then
                    name="${BASH_REMATCH[1]}"
                elif [[ "$line" =~ ^[[:space:]]*path:[[:space:]]*(.*) ]]; then
                    path="${BASH_REMATCH[1]}"
                    # Strip ./grafana-dashboards/ prefix, map to container path
                    local rel="${path#./grafana-dashboards}"
                    rel="${rel%/}"
                    cat <<ENTRY
  - name: "${name}"
    folder: "${name}"
    type: file
    options:
      path: /var/lib/grafana/dashboards${rel}
ENTRY
                fi
            fi
        done < "$obs_config"
    } > "${tmpdir}/provisioning/dashboards/dashboards.yaml"

    # Datasource provisioning — uses Bearer token auth with the az CLI token.
    # The token expires (~1h), so restart the container or run this script again
    # to refresh it.
    local svc_ds_name="Managed_Prometheus_${SVC_WORKSPACE_NAME}"
    local hcp_ds_name="Managed_Prometheus_${HCP_WORKSPACE_NAME}"

    cat > "${tmpdir}/provisioning/datasources/datasources.yaml" <<YAML
apiVersion: 1
datasources:
  - name: ${svc_ds_name}
    type: prometheus
    access: proxy
    url: ${svc_endpoint}
    isDefault: true
    jsonData:
      httpHeaderName1: Authorization
      timeInterval: 60s
    secureJsonData:
      httpHeaderValue1: "Bearer ${token}"
    editable: true

  - name: ${hcp_ds_name}
    type: prometheus
    access: proxy
    url: ${hcp_endpoint}
    jsonData:
      httpHeaderName1: Authorization
      timeInterval: 60s
    secureJsonData:
      httpHeaderValue1: "Bearer ${token}"
    editable: true
YAML

    log "Starting Grafana ${GRAFANA_VERSION} on port ${GRAFANA_PORT}..."
    docker run -d \
        --name "${CONTAINER_NAME}" \
        -p "${GRAFANA_PORT}:3000" \
        -e GF_AUTH_ANONYMOUS_ENABLED=true \
        -e GF_AUTH_ANONYMOUS_ORG_ROLE=Admin \
        -e GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/var/lib/grafana/dashboards/sre/user-journey/mgmt-cluster-triage.json \
        -v "${REPO_ROOT}/observability/grafana-dashboards:/var/lib/grafana/dashboards:ro" \
        -v "${tmpdir}/provisioning/dashboards:/etc/grafana/provisioning/dashboards:ro" \
        -v "${tmpdir}/provisioning/datasources:/etc/grafana/provisioning/datasources:ro" \
        "grafana/grafana:${GRAFANA_VERSION}" >/dev/null

    # Wait briefly for the container to start, then detach the tmpdir trap
    # by copying provisioning into the container so it persists.
    sleep 2
    docker cp "${tmpdir}/provisioning" "${CONTAINER_NAME}:/etc/grafana/provisioning-backup" >/dev/null 2>&1 || true

    log ""
    log "Grafana is running at: http://localhost:${GRAFANA_PORT}"
    log ""
    log "Datasources configured:"
    log "  - ${svc_ds_name}  (SVC AMW)"
    log "  - ${hcp_ds_name}  (HCP AMW)"
    log ""
    log "NOTE: The Azure token expires in ~1 hour. Run '$(basename "$0") --restart'"
    log "      to refresh it."
}

case "${1:---start}" in
    --start)   start_grafana ;;
    --stop)    stop_grafana ;;
    --restart) stop_grafana; start_grafana ;;
    --status)  status_grafana ;;
    -h|--help) usage ;;
    *)         usage; exit 1 ;;
esac
