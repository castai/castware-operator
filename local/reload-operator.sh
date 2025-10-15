#!/bin/bash
set -e

# Configuration
CLUSTER_NAME="castware-operator"
IMAGE_NAME="castai/castware-operator"
IMAGE_TAG="${IMAGE_TAG:-local-dev}"
NAMESPACE="castai-agent"

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${BLUE}=== Rebuilding Operator Binary & Docker Image ===${NC}"
make build
make docker-build IMG=${IMAGE_NAME}:${IMAGE_TAG}

echo -e "${BLUE}=== Loading Image into Kind Cluster ===${NC}"
kind load docker-image ${IMAGE_NAME}:${IMAGE_TAG} --name ${CLUSTER_NAME}

echo -e "${BLUE}=== Restarting Operator Pod ===${NC}"
kubectl rollout restart deployment -n ${NAMESPACE} -l app.kubernetes.io/name=castware-operator

echo -e "${BLUE}=== Waiting for Rollout to Complete ===${NC}"
kubectl rollout status deployment -n ${NAMESPACE} -l app.kubernetes.io/name=castware-operator --timeout=120s

echo -e "${GREEN}=== Operator Reloaded Successfully ===${NC}"
echo ""
echo "Useful commands:"
echo "  Watch pods: kubectl get pods -n ${NAMESPACE} -w"
echo "  View logs:  kubectl logs -n ${NAMESPACE} -l app.kubernetes.io/name=castware-operator -f"
