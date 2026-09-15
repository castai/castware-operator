#!/bin/bash
set -e

# Flags
EXTENDED_PERMISSIONS=false
UMBRELLA=false
UMBRELLA_TAG=""
while [[ $# -gt 0 ]]; do
	case $1 in
	--extended-permissions)
		EXTENDED_PERMISSIONS=true
		shift
		;;
	--umbrella)
		UMBRELLA=true
		shift
		;;
	--tag=*)
		UMBRELLA_TAG="${1#*=}"
		shift
		;;
	*)
		echo "Unknown flag: $1"
		echo "Usage: $0 [--extended-permissions] [--umbrella] [--tag=<readonly|node-autoscaler|workload-autoscaler|full|autoscaler-anywhere|autoscaler-openshift>]"
		echo "  --extended-permissions  Enable extended RBAC permissions (phase2 components)"
		echo "  --umbrella              Use umbrella component path instead of individual charts"
		echo "  --tag                   Umbrella mode tag (requires --umbrella; non-readonly tags auto-enable extended permissions)"
		exit 1
		;;
	esac
done

# Configuration
CLUSTER_NAME="castware-operator"
IMAGE_NAME="castai/castware-operator"
IMAGE_TAG="${IMAGE_TAG:-local-dev}"
NAMESPACE="castai-agent"
RELEASE_NAME="castware-operator"
export GOOS=linux
export GOARCH="${GOARCH:-arm64}"

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Validate the umbrella mode tag (only one mode tag may be enabled at a time
# in the umbrella chart). Any non-readonly mode pulls in the cluster-controller
# (woop), which the umbrella webhook only admits when extended permissions are
# installed, so enable them automatically.
if [ -n "${UMBRELLA_TAG}" ]; then
	if [ "${UMBRELLA}" != "true" ]; then
		echo "--tag=${UMBRELLA_TAG} requires --umbrella"
		exit 1
	fi
	case "${UMBRELLA_TAG}" in
	readonly | node-autoscaler | workload-autoscaler | full | autoscaler-anywhere | autoscaler-openshift) ;;
	*)
		echo "Invalid --tag value: ${UMBRELLA_TAG}"
		echo "Allowed: readonly, node-autoscaler, workload-autoscaler, full, autoscaler-anywhere, autoscaler-openshift"
		exit 1
		;;
	esac
	if [ "${UMBRELLA_TAG}" != "readonly" ] && [ "${EXTENDED_PERMISSIONS}" != "true" ]; then
		echo -e "${YELLOW}--tag=${UMBRELLA_TAG} requires extended permissions, enabling --extended-permissions${NC}"
		EXTENDED_PERMISSIONS=true
	fi
fi

echo -e "${BLUE}=== Building Operator Binary & Docker Image ===${NC}"
go build -o bin/castware-operator-${GOARCH} github.com/castai/castware-operator/cmd
make docker-build IMG=${IMAGE_NAME}:${IMAGE_TAG}

echo -e "${BLUE}=== Loading Image into Kind Cluster ===${NC}"
kind load docker-image ${IMAGE_NAME}:${IMAGE_TAG} --name ${CLUSTER_NAME}

echo -e "${BLUE}=== Checking if release already exists ===${NC}"
if helm list -n ${NAMESPACE} | grep -q ${RELEASE_NAME}; then
	echo -e "${YELLOW}Found existing release, uninstalling...${NC}"
	helm uninstall ${RELEASE_NAME} -n ${NAMESPACE} || true
	echo "Waiting for resources to be cleaned up..."
	sleep 5
fi

# Build extra helm args for extended permissions and umbrella path
EXTRA_HELM_ARGS=()
if [ "${UMBRELLA}" = "true" ]; then
	EXTRA_HELM_ARGS+=(
		--set "defaultComponents.umbrella.enabled=true"
		# The umbrella chart nests every sub-component under the autoscaler
		# sub-chart, so agent overrides must use the autoscaler.castai-agent
		# path (a top-level castai-agent block is ignored by the chart).
		--set "defaultComponents.umbrella.overrides.autoscaler.castai-agent.additionalEnv.GKE_CLUSTER_NAME=${GKE_CLUSTER_NAME:-castware-operator-test}"
		--set "defaultComponents.umbrella.overrides.autoscaler.castai-agent.additionalEnv.GKE_LOCATION=${GKE_LOCATION:-local}"
		--set "defaultComponents.umbrella.overrides.autoscaler.castai-agent.additionalEnv.GKE_PROJECT_ID=${GKE_PROJECT_ID:-local-test}"
		--set "defaultComponents.umbrella.overrides.autoscaler.castai-agent.additionalEnv.GKE_REGION=${GKE_REGION:-local1}"
	)
	if [ -n "${UMBRELLA_TAG}" ]; then
		EXTRA_HELM_ARGS+=(
			--set "defaultComponents.umbrella.tags.${UMBRELLA_TAG}=true"
		)
	fi
else
	EXTRA_HELM_ARGS+=(
		--set "defaultComponents.components.castai-agent.overrides.additionalEnv.GKE_CLUSTER_NAME=${GKE_CLUSTER_NAME:-castware-operator-test}"
		--set "defaultComponents.components.castai-agent.overrides.additionalEnv.GKE_LOCATION=${GKE_LOCATION:-local}"
		--set "defaultComponents.components.castai-agent.overrides.additionalEnv.GKE_PROJECT_ID=${GKE_PROJECT_ID:-local-test}"
		--set "defaultComponents.components.castai-agent.overrides.additionalEnv.GKE_REGION=${GKE_REGION:-local1}"
	)
	if [ "${EXTENDED_PERMISSIONS}" = "true" ]; then
		EXTRA_HELM_ARGS+=(
			--set "defaultComponents.components.cluster-controller.component=cluster-controller"
			--set "defaultComponents.components.cluster-controller.cluster=castai"
			--set "defaultComponents.components.cluster-controller.enabled=true"
		)
	fi
fi

echo -e "${BLUE}=== Labeling kind node as spot (spot-handler requires a spot label) ===${NC}"
NODE_NAME=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [ -n "${NODE_NAME}" ]; then
	kubectl label node "${NODE_NAME}" --overwrite cloud.google.com/gke-spot=true >/dev/null 2>&1 || echo "${YELLOW}Warning: could not label node ${NODE_NAME} as spot${NC}"
else
	echo "${YELLOW}Warning: no nodes found, skipping spot label${NC}"
fi

echo -e "${BLUE}=== Installing Helm Chart ===${NC}"
helm upgrade --install ${RELEASE_NAME} \
	--namespace ${NAMESPACE} --create-namespace \
	--set image.repository=${IMAGE_NAME} \
	--set image.tag=${IMAGE_TAG} \
	--set image.pullPolicy=IfNotPresent \
	--set apiKeySecret.apiKey="${API_KEY}" \
	--set defaultCluster.provider="${PROVIDER:-gke}" \
	--set defaultCluster.api.apiUrl="${API_URL}" \
	--set defaultCluster.terraform=false \
	--set webhook.env.GKE_CLUSTER_NAME="${GKE_CLUSTER_NAME:-castware-operator-test}" \
	--set webhook.env.GKE_LOCATION="${GKE_LOCATION:-local}" \
	--set webhook.env.GKE_PROJECT_ID="${GKE_PROJECT_ID:-local-test}" \
	--set webhook.env.GKE_REGION="${GKE_REGION:-local1}" \
	--set extendedPermissions=${EXTENDED_PERMISSIONS} \
	"${EXTRA_HELM_ARGS[@]}" \
	--atomic \
	--timeout 5m \
	./charts/castai-castware-operator

echo -e "${GREEN}=== Installation Complete ===${NC}"
echo ""
echo "Useful commands:"
echo "  Watch pods: kubectl get pods -n ${NAMESPACE} -w"
echo "  View logs:  kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castware-operator -f"
echo "  Get status: helm status ${RELEASE_NAME} -n ${NAMESPACE}"
echo "  Uninstall:  helm uninstall ${RELEASE_NAME} -n ${NAMESPACE}"
