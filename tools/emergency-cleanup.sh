#!/bin/bash

#Cleanup script to run if operator helm uninstall fails and the namespace is stuck on "Terminating" status

set -e  # Exit on error

echo "Checking for existing castai-agent pod..."
if kubectl get pods -n castai-agent -l app.kubernetes.io/name=castai-agent --no-headers 2>/dev/null | grep -q .; then
    echo "ERROR: Pod with label app.kubernetes.io/name=castai-agent exists in castai-agent namespace"
    echo "Please remove the pod before running this script"
    exit 1
fi
echo "No castai-agent pod found, continuing..."

echo "Deleting castware-operator-mutating-webhook-configuration..."
if kubectl delete mutatingwebhookconfiguration castware-operator-mutating-webhook-configuration 2>/dev/null; then
    echo "Mutating webhook configuration deleted"
else
    echo "Mutating webhook configuration not found or already deleted"
fi

echo "Deleting castware-operator-validating-webhook-configuration..."
if kubectl delete validatingwebhookconfiguration castware-operator-validating-webhook-configuration 2>/dev/null; then
    echo "Validating webhook configuration deleted"
else
    echo "Validating webhook configuration not found or already deleted"
fi

echo "Checking for castware-operator Helm chart..."
if helm list -n castai-agent --no-headers 2>/dev/null | grep -q "^castware-operator"; then
    echo "Found castware-operator Helm chart, uninstalling..."
    helm uninstall castware-operator -n castai-agent --no-hooks
    echo "Helm chart uninstalled successfully"
else
    echo "castware-operator Helm chart not found"
fi

echo "Removing finalizers from Component custom resources..."
components=$(kubectl get components -n castai-agent --no-headers -o custom-columns=":metadata.name" 2>/dev/null || true)

if [ -n "$components" ]; then
    while IFS= read -r component; do
        echo "Processing component: $component"
        kubectl patch component "$component" -n castai-agent \
            --type=json \
            -p='[{"op": "remove", "path": "/metadata/finalizers", "value": ["castware.cast.ai/cleanup-helm"]}]' 2>/dev/null || \
        kubectl patch component "$component" -n castai-agent \
            --type=json \
            -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || \
        echo "No finalizers found on component $component"
    done <<< "$components"
    echo "Finalizers removed from all Component resources"
else
    echo "No Component resources found in castai-agent namespace"
fi

echo "Deleting castai-agent namespace..."
if kubectl delete namespace castai-agent 2>/dev/null; then
    echo "Namespace castai-agent deleted successfully"
else
    echo "Namespace castai-agent not found or already deleted"
fi

echo "Cleanup completed successfully!"

