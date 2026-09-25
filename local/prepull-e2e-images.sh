#!/usr/bin/env bash
# Pre-pull the CAST AI component images the e2e suite installs and load them
# into the kind node, so installs do not pull them serially mid-test.
#
# The image set is derived by rendering the published umbrella chart with the
# full tag (a superset of every workload the suite runs except castai-live,
# which is disabled by the operator's own defaults) plus the operator chart
# (the crd-upgrade job's kubectl image) and the harness's own images
# (chartmuseum, pause, curl). Versions are resolved from the current chart, so
# the list tracks chart releases without manual updates.
#
# The local operator image (cast.ai/castware-operator) is excluded: the suite
# builds and loads it itself.
#
# Pull failures are warned and skipped (best effort): a single image without
# the host platform would otherwise abort the whole run; the install path
# pulls it again anyway if it can.
#
# Usage: [KIND_CLUSTER=<name>] ./local/prepull-e2e-images.sh
set -euo pipefail

CLUSTER="${KIND_CLUSTER:-castware-operator}"
HELM_BIN="${HELM_BIN:-helm}"

log() { printf '[prepull] %s\n' "$*"; }

if ! command -v "$HELM_BIN" >/dev/null 2>&1; then
    echo "helm not found" >&2
    exit 1
fi
if ! command -v docker >/dev/null 2>&1; then
    echo "docker not found" >&2
    exit 1
fi

TMPDIR_RENDER="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_RENDER"' EXIT

log "adding the castai-helm repo"
"$HELM_BIN" repo add castai-helm https://castai.github.io/helm-charts --force-update >/dev/null
"$HELM_BIN" repo update castai-helm >/dev/null

log "rendering the umbrella chart (full tag) to resolve images"
"$HELM_BIN" template prepull castai-helm/castai \
    --set tags.full=true \
    --set global.castai.provider=gke \
    --set global.castai.apiURL=https://unused.invalid \
    >"$TMPDIR_RENDER/umbrella.yaml" 2>/dev/null

log "rendering the operator chart to resolve job images"
"$HELM_BIN" template charts/castai-castware-operator \
    --set apiKeySecret.apiKey=unused \
    >"$TMPDIR_RENDER/operator.yaml" 2>/dev/null || true

# Image references from the rendered manifests, plus the harness's own images.
# The operator's own image is excluded (the suite builds and loads it).
images="$(
    {
        grep -hoE 'image: *"?[^" ]+"?' "$TMPDIR_RENDER"/umbrella.yaml "$TMPDIR_RENDER"/operator.yaml 2>/dev/null
        echo 'image: ghcr.io/helm/chartmuseum:v0.16.0'
        echo 'image: registry.k8s.io/pause:3.10'
        echo 'image: curlimages/curl:latest'
    } |
        sed -E 's/^image: *"?([^" ]+)"?$/\1/' |
        grep -v 'castware-operator' |
        sort -u
)"

count="$(printf '%s\n' "$images" | wc -l | tr -d ' ')"
log "pulling $count images in parallel"
printf '%s\n' "$images" | xargs -P 8 -I{} sh -c 'docker pull -q "{}" >/dev/null 2>&1 || echo "[prepull] WARNING: pull failed (skipped): {}" >&2'

log "loading images into the kind cluster $CLUSTER"
failed=0
while IFS= read -r image; do
    if ! kind load docker-image "$image" --name "$CLUSTER" >/dev/null 2>&1; then
        echo "[prepull] WARNING: kind load failed (skipped): $image" >&2
        failed=1
    fi
done <<EOF
$images
EOF

if [ "$failed" -ne 0 ]; then
    log "done with warnings (some images could not be loaded)"
else
    log "done"
fi
