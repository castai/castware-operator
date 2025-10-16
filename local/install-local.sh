#!/bin/bash
set -e

# Configuration
CLUSTER_NAME="castware-operator"
IMAGE_NAME="castai/castware-operator"
IMAGE_TAG="${IMAGE_TAG:-local-dev}"
NAMESPACE="castai-agent"
RELEASE_NAME="castware-operator"

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${BLUE}=== Building Operator Binary & Docker Image ===${NC}"
make build
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

echo -e "${BLUE}=== Installing Helm Chart ===${NC}"
helm install ${RELEASE_NAME} \
    --namespace ${NAMESPACE} --create-namespace \
    --set image.repository=${IMAGE_NAME} \
    --set image.tag=${IMAGE_TAG} \
    --set image.pullPolicy=IfNotPresent \
    --set apiKeySecret.apiKey="${API_KEY_BASE64}" \
    --set defaultCluster.provider="${PROVIDER:-gke}" \
    --set defaultCluster.api.apiUrl="${API_URL}" \
    --set webhook.env.GKE_CLUSTER_NAME="${GKE_CLUSTER_NAME:-castware-operator-test}" \
    --set webhook.env.GKE_LOCATION="${GKE_LOCATION:-local}" \
    --set webhook.env.GKE_PROJECT_ID="${GKE_PROJECT_ID:-local-test}" \
    --set webhook.env.GKE_REGION="${GKE_REGION:-local1}" \
    --wait \
    --timeout 5m \
    ./charts/castai-castware-operator

echo -e "${GREEN}=== Installation Complete ===${NC}"
echo ""
echo "Useful commands:"
echo "  Watch pods: kubectl get pods -n ${NAMESPACE} -w"
echo "  View logs:  kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castware-operator -f"
echo "  Get status: helm status ${RELEASE_NAME} -n ${NAMESPACE}"
echo "  Uninstall:  helm uninstall ${RELEASE_NAME} -n ${NAMESPACE}"
