#!/bin/bash
set -e

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

echo -e "${BLUE}=== Building Operator Binary & Docker Image ===${NC}"
go build -o bin/castware-operator-${GOARCH} github.com/castai/castware-operator/cmd
make docker-build IMG=${IMAGE_NAME}:${IMAGE_TAG}

echo -e "${BLUE}=== Loading Image into Kind Cluster ===${NC}"
kind load docker-image ${IMAGE_NAME}:${IMAGE_TAG} --name ${CLUSTER_NAME}

echo -e "${BLUE}=== Extending Helm Chart ===${NC}"
helm upgrade castware-operator -n castai-agent --atomic \
      --set extendedPermissions="false" \
      --reset-then-reuse-values \
      ./charts/castai-castware-operator

echo -e "${GREEN}=== Installation Complete ===${NC}"
echo ""
echo "Useful commands:"
echo "  Watch pods: kubectl get pods -n ${NAMESPACE} -w"
echo "  View logs:  kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castware-operator -f"
echo "  Get status: helm status ${RELEASE_NAME} -n ${NAMESPACE}"
echo "  Uninstall:  helm uninstall ${RELEASE_NAME} -n ${NAMESPACE}"
