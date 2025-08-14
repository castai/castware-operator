#!/usr/bin/env bash
set -euo pipefail

LOCAL_DIR="$(dirname "$(realpath "$0")")"

# -------- Helpers --------
log() { printf "\033[1;32m[✔]\033[0m %s\n" "$*"; }
warn() { printf "\033[1;33m[!]\033[0m %s\n" "$*" >&2; }
err() { printf "\033[1;31m[✖]\033[0m %s\n" "$*" >&2; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || { err "Required command '$1' not found."; exit 1; }; }

# -------- 1 & 2) Check env vars --------
: "${API_KEY:?API_KEY environment variable is not set}"
: "${API_URL:?API_URL environment variable is not set}"
log "Environment variables API_KEY and API_URL are set."

export GKE_CLUSTER_NAME=castware-operator-test
export GKE_LOCATION=local
export GKE_PROJECT_ID=local-test
export GKE_REGION=local1
export POD_NAMESPACE=castai-agent
export REQUEST_TIMEOUT=10m
export WEBHOOK_PORT=9443
export CERTS_DIR="${LOCAL_DIR}/certs"
export CERTS_ROTATION=true
export NAMESPACE=castai-agent
export CERTS_SECRET_NAME="castware-operator-certs"
export WEBHOOK_SERVICE_DNS_NAME=host.docker.internal

# -------- 3) Ensure yq is installed (latest) --------
install_yq_latest() {
  require_cmd curl
  require_cmd grep
  require_cmd sed
  local os arch tag asset url tmpbin install_dir

  case "$(uname -s)" in
    Linux)  os="linux" ;;
    Darwin) os="darwin" ;;
    *) err "Unsupported OS: $(uname -s). yq binaries available for Linux/Darwin."; exit 1 ;;
  esac

  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) err "Unsupported architecture: $(uname -m)."; exit 1 ;;
  esac

  tag="$(curl -fsSL https://api.github.com/repos/mikefarah/yq/releases/latest \
        | grep -Eo '"tag_name":\s*"v?[^"]+"' \
        | sed -E 's/.*"v?([^"]+)".*/\1/' )"
  [ -n "$tag" ] || { err "Unable to determine latest yq release tag."; exit 1; }

  asset="yq_${os}_${arch}"
  url="https://github.com/mikefarah/yq/releases/download/v${tag}/${asset}"

  tmpbin="$(mktemp)"
  trap 'rm -f "$tmpbin"' EXIT
  curl -fsSL "$url" -o "$tmpbin"
  chmod +x "$tmpbin"

  if [ -w /usr/local/bin ]; then
    install_dir="/usr/local/bin"
  else
    install_dir="${HOME}/.local/bin"
    mkdir -p "$install_dir"
  fi

  mv "$tmpbin" "${install_dir}/yq"
  log "Installed yq v${tag} to ${install_dir}/yq"
}

if command -v yq >/dev/null 2>&1; then
  log "yq already installed: $(yq --version 2>/dev/null || echo 'unknown version')"
else
  warn "yq not found. Installing latest release…"
  install_yq_latest
fi

# -------- 4) Ensure kind cluster exists --------
require_cmd kind
require_cmd kubectl

CLUSTER_NAME="castware-operator"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  log "kind cluster '$CLUSTER_NAME' already exists."
else
  warn "kind cluster '$CLUSTER_NAME' not found. Creating…"
  kind create cluster --name "$CLUSTER_NAME"
  log "Created kind cluster '$CLUSTER_NAME'."
fi

# -------- 5) Switch kube context to this cluster --------
CONTEXT="kind-${CLUSTER_NAME}"
kubectl config use-context "$CONTEXT" >/dev/null
log "Switched kubectl context to '$CONTEXT'."

# -------- 6) Optionally run `make build-installer` --------
if [ -z "${MAKELEVEL:-}" ]; then
  if command -v make >/dev/null 2>&1; then
    if [ -f Makefile ] || [ -f makefile ] || [ -f GNUmakefile ]; then
      if make -q build-installer >/dev/null 2>&1; then
        log "Target 'build-installer' is up to date."
      else
        warn "Running 'make build-installer'…"
        make build-installer
        log "'make build-installer' completed."
      fi
    else
      warn "No Makefile found; skipping 'make build-installer'."
    fi
  else
    warn "make not installed; skipping 'make build-installer'."
  fi
else
  log "Detected invocation from make; skipping 'make build-installer' per instructions."
fi

# -------- 7) Delete certs secret if exists and apply manifests --------
if kubectl get secret "$CERTS_SECRET_NAME" -n "$NAMESPACE" >/dev/null 2>&1; then
    warn "Secret '$CERTS_SECRET_NAME' found, deleting it before recreating"
    kubectl delete secret "$CERTS_SECRET_NAME" -n "$NAMESPACE"
fi
kubectl apply -f $LOCAL_DIR/../dist/install.yaml

# -------- 8) Delete the deployment castware-operator-controller-manager --------
DEPLOY_NAME="castware-operator-controller-manager"
ns="$(kubectl get deploy --all-namespaces -o jsonpath='{range .items[?(@.metadata.name=="castware-operator-controller-manager")]}{.metadata.namespace}{"\n"}{end}' | head -n1 || true)"

if [ -n "${ns:-}" ]; then
  warn "Deleting deployment '$DEPLOY_NAME' in namespace '$ns'…"
  kubectl delete deployment "$DEPLOY_NAME" -n "$ns" --ignore-not-found
  log "Deleted deployment '$DEPLOY_NAME' (if it existed) in namespace '$ns'."
else
  warn "Deployment '$DEPLOY_NAME' not found in any namespace; nothing to delete."
fi


# -------- 9) Kustomize-based secret patch/apply/cleanup --------
SECRET_FILE="${LOCAL_DIR}/secret.yaml"
KUSTOMIZATION_FILE="${LOCAL_DIR}/kustomization.yaml"

echo "API_KEY=$API_KEY" > "${LOCAL_DIR}/secret.env"
kubectl apply -k "$LOCAL_DIR"
log "Applied kustomization from $LOCAL_DIR."

log "All done, starting operator."

# -------- 10) Create sample cluster custom resource with api url --------
mkdir -p ${LOCAL_DIR}/samples
cp "${LOCAL_DIR}/../config/samples/castware_v1alpha1_cluster.yaml" "${LOCAL_DIR}/samples/castware_v1alpha1_cluster.yaml"
yq -i '.spec.api.apiUrl = env(API_URL)' "${LOCAL_DIR}/samples/castware_v1alpha1_cluster.yaml"
log "Sample cluster file patched"

cp "${LOCAL_DIR}/../config/samples/castware_v1alpha1_component.yaml" "${LOCAL_DIR}/samples/castware_v1alpha1_component.yaml"
yq -i '
  .spec.values.additionalEnv = {
    "GKE_CLUSTER_NAME": env(GKE_CLUSTER_NAME),
    "GKE_LOCATION": env(GKE_LOCATION),
    "GKE_PROJECT_ID": env(GKE_PROJECT_ID),
    "GKE_REGION": env(GKE_REGION)
  }
' "${LOCAL_DIR}/samples/castware_v1alpha1_component.yaml"

log "Sample component file patched"


# -------- 11) Create cert dir and start operator --------
rm -rf ${CERTS_DIR}
mkdir -p ${CERTS_DIR}
cd ${LOCAL_DIR}/..
go run cmd/main.go
log "Webhook certificates configured, restarting."

# -------- 12) Load certs from secret and restart --------
if ! kubectl get secret "$CERTS_SECRET_NAME" -n "$NAMESPACE" >/dev/null 2>&1; then
    err "Secret '$CERTS_SECRET_NAME' not found in namespace '$NAMESPACE'."
    exit 1
fi

log "Copying certificates from secret $CERTS_SECRET_NAME in ${CERTS_DIR}"
kubectl get secret "$CERTS_SECRET_NAME" -n "$NAMESPACE" -o yaml \
| yq -r '.data | to_entries[] | [.key, .value] | @tsv' \
| while IFS=$'\t' read -r key b64; do
    # Skip safety: empty key
    [ -z "$key" ] && continue
    printf "%s" "$b64" | base64 --decode > "${CERTS_DIR}/${key}"
    log "Saved ${CERTS_DIR}/${key}"
  done

log "Restarting operator"
go run cmd/main.go

