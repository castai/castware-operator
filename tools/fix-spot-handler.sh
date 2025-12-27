#!/bin/bash
set -euo pipefail

NAMESPACE="castai-agent"
COMPONENT_NAME="spot-handler"
HELM_RELEASE_NAME="spot-handler"

echo "Checking if component '$COMPONENT_NAME' exists in namespace '$NAMESPACE'..."
if ! kubectl get component "$COMPONENT_NAME" -n "$NAMESPACE" &>/dev/null; then
    echo "Component '$COMPONENT_NAME' does not exist. Exiting."
    exit 0
fi

echo "Extracting cluster configuration from 'castai' cluster resource..."
CASTAI_CLUSTER_ID=$(kubectl get cluster castai -n "$NAMESPACE" -o jsonpath='{.spec.cluster.clusterID}')
API_KEY_SECRET=$(kubectl get cluster castai -n "$NAMESPACE" -o jsonpath='{.spec.apiKeySecret}')
CASTAI_API_URL=$(kubectl get cluster castai -n "$NAMESPACE" -o jsonpath='{.spec.api.apiUrl}')
CLUSTER_PROVIDER=$(kubectl get cluster castai -n "$NAMESPACE" -o jsonpath='{.spec.provider}')

if [[ -z "$CASTAI_CLUSTER_ID" ]]; then
    echo "Failed to extract clusterId. Exiting."
    exit 1
fi

if [[ -z "$CASTAI_API_URL" ]]; then
    echo "Failed to extract apiUrl. Exiting."
    exit 1
fi

if [[ -z "$CLUSTER_PROVIDER" ]]; then
    echo "Failed to extract provider. Exiting."
    exit 1
fi

if [[ -z "$API_KEY_SECRET" ]]; then
    echo "Failed to extract api key secret name. Exiting."
    exit 1
fi

echo "Extracting API key..."
if kubectl get secret $API_KEY_SECRET -n "$NAMESPACE" &>/dev/null; then
    echo "Using $API_KEY_SECRET secret"
    CASTAI_API_TOKEN=$(kubectl get secret castware-api-key -n "$NAMESPACE" -o jsonpath='{.data.API_KEY}' | base64 -d)
elif kubectl get secret castai-agent -n "$NAMESPACE" &>/dev/null; then
    echo "Using castai-agent secret"
    CASTAI_API_TOKEN=$(kubectl get secret castai-agent -n "$NAMESPACE" -o jsonpath='{.data.API_KEY}' | base64 -d)
else
    echo "No API key secret found. Exiting."
    exit 1
fi

if [[ -z "$CASTAI_API_TOKEN" ]]; then
    echo "API key is empty. Exiting."
    exit 1
fi

case "$CLUSTER_PROVIDER" in
    eks) CASTAI_PROVIDER="aws" ;;
    gke) CASTAI_PROVIDER="gcp" ;;
    aks) CASTAI_PROVIDER="azure" ;;
    *)
        echo "Unknown provider '$CLUSTER_PROVIDER'. Exiting."
        exit 1
        ;;
esac

# Delete ALL helm release secrets for spot-handler
kubectl delete secret -n castai-agent -l name=spot-handler,owner=helm
echo "$COMPONENT_NAME secret deleted"

# Delete the resources the operator can't delete
kubectl patch clusterrole castware-operator-manager-role --type=json -p='[
  {"op": "add", "path": "/rules/-", "value": {"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["clusterroles", "clusterrolebindings"], "verbs": ["delete"]}}
]'
echo "castware-operator-manager-role patched"

kubectl delete clusterrolebinding castai-spot-handler
kubectl delete clusterrole castai-spot-handler
kubectl delete serviceaccount -n $NAMESPACE castai-spot-handler
kubectl delete daemonset -n $NAMESPACE castai-spot-handler

echo "Component '$COMPONENT_NAME' found. Deleting..."
kubectl delete component "$COMPONENT_NAME" -n "$NAMESPACE"

echo "Waiting for helm release '$HELM_RELEASE_NAME' to be uninstalled..."
while helm status "$HELM_RELEASE_NAME" -n "$NAMESPACE" &>/dev/null; do
    echo "Helm release still exists. Waiting..."
    sleep 5
done
echo "Helm release '$HELM_RELEASE_NAME' has been uninstalled."

echo "Installing castai-spot-handler helm chart..."
echo "  Cluster ID: $CASTAI_CLUSTER_ID"
echo "  API URL: $CASTAI_API_URL"
echo "  Provider: $CLUSTER_PROVIDER -> $CASTAI_PROVIDER"

helm upgrade -i castai-spot-handler castai-helm/castai-spot-handler -n "$NAMESPACE" \
    --set castai.apiKey="$CASTAI_API_TOKEN" \
    --set castai.apiURL="$CASTAI_API_URL" \
    --set castai.clusterID="$CASTAI_CLUSTER_ID" \
    --set castai.provider="$CASTAI_PROVIDER" \
    --set castai.clusterIdConfigMapKeyRef.name=null \
    --set castai.clusterIdSecretKeyRef.name=null

echo "Done!"