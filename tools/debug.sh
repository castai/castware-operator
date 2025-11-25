#!/bin/bash

# Castware Operator Debug Data Collection Script

OUTPUT_DIR="castware-operator-debug-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUTPUT_DIR"

echo "Collecting Castware Operator debug information..."
echo "Output directory: $OUTPUT_DIR"

# Operator Pod Status and Logs
echo "=== Collecting Operator Pod Information ==="
kubectl get pods -n castai-agent -l app.kubernetes.io/name=castware-operator -o wide > "$OUTPUT_DIR/operator-pods.txt" 2>&1
kubectl describe pods -n castai-agent -l app.kubernetes.io/name=castware-operator > "$OUTPUT_DIR/operator-pods-describe.txt" 2>&1
kubectl logs -n castai-agent -l app.kubernetes.io/name=castware-operator --all-containers --tail=500 > "$OUTPUT_DIR/operator-logs.txt" 2>&1
kubectl logs -n castai-agent -l app.kubernetes.io/name=castware-operator --all-containers --previous --tail=500 > "$OUTPUT_DIR/operator-logs-previous.txt" 2>&1 || echo "No previous logs available"

# CRD Status
echo "=== Collecting CRD Information ==="
kubectl get crds -o jsonpath='{range .items[?(@.spec.group=="castware.cast.ai")]}{.metadata.name}{"\n"}{end}' > "$OUTPUT_DIR/castware-crds-list.txt" 2>&1
kubectl get crds clusters.castware.cast.ai -o yaml > "$OUTPUT_DIR/crd-cluster.yaml" 2>&1 || echo "Cluster CRD not found" > "$OUTPUT_DIR/crd-cluster.yaml"
kubectl get crds components.castware.cast.ai -o yaml > "$OUTPUT_DIR/crd-component.yaml" 2>&1 || echo "Component CRD not found" > "$OUTPUT_DIR/crd-component.yaml"

# Custom Resources (Cluster and Component)
echo "=== Collecting Custom Resources ==="
kubectl get cluster -n castai-agent -o yaml > "$OUTPUT_DIR/cluster-cr.yaml" 2>&1
kubectl describe cluster -n castai-agent > "$OUTPUT_DIR/cluster-cr-describe.txt" 2>&1
kubectl get components -n castai-agent -o yaml > "$OUTPUT_DIR/components-cr.yaml" 2>&1
kubectl describe components -n castai-agent > "$OUTPUT_DIR/components-cr-describe.txt" 2>&1

# Operator Deployment and Configuration
echo "=== Collecting Operator Deployment ==="
kubectl get deployment -n castai-agent castware-operator -o yaml > "$OUTPUT_DIR/operator-deployment.yaml" 2>&1
kubectl describe deployment -n castai-agent castware-operator > "$OUTPUT_DIR/operator-deployment-describe.txt" 2>&1

# Webhook Configuration
echo "=== Collecting Webhook Information ==="
kubectl get validatingwebhookconfigurations -o yaml | grep -A 50 "castware" > "$OUTPUT_DIR/validating-webhooks.yaml" 2>&1
kubectl get mutatingwebhookconfigurations -o yaml | grep -A 50 "castware" > "$OUTPUT_DIR/mutating-webhooks.yaml" 2>&1
kubectl get clusterrole castware-operator-crd-upgrade -o yaml > "$OUTPUT_DIR/operator-crd-upgrade-role.yaml" 2>&1 || echo "CRD upgrade role not found"

echo "=== Collecting RBAC Information ==="
kubectl get serviceaccount -n castai-agent castware-operator-controller-manager -o yaml > "$OUTPUT_DIR/operator-serviceaccount-controller.yaml" 2>&1
kubectl get serviceaccount -n castai-agent castware-operator-crd-upgrade -o yaml > "$OUTPUT_DIR/operator-serviceaccount-crd-upgrade.yaml" 2>&1
kubectl get role,rolebinding -n castai-agent -o yaml > "$OUTPUT_DIR/operator-rbac-namespace.yaml" 2>&1
kubectl get clusterrole -o name | grep castware-operator | xargs -I {} kubectl get {} -o yaml > "$OUTPUT_DIR/operator-clusterroles.yaml" 2>&1
kubectl get clusterrolebinding -o name | grep castware-operator | xargs -I {} kubectl get {} -o yaml > "$OUTPUT_DIR/operator-clusterrolebindings.yaml" 2>&1
kubectl get clusterrole castware-operator-crd-upgrade -o yaml > "$OUTPUT_DIR/operator-crd-upgrade-role.yaml" 2>&1 || echo "CRD upgrade role not found" > "$OUTPUT_DIR/operator-crd-upgrade-role.yaml"
kubectl get clusterrolebinding castware-operator-crd-upgrade -o yaml > "$OUTPUT_DIR/operator-crd-upgrade-rolebinding.yaml" 2>&1 || echo "CRD upgrade rolebinding not found" > "$OUTPUT_DIR/operator-crd-upgrade-rolebinding.yaml"
kubectl get clusterrole castware-operator-manager-role -o yaml > "$OUTPUT_DIR/operator-manager-role.yaml" 2>&1 || echo "Manager role not found" > "$OUTPUT_DIR/operator-manager-role.yaml"
kubectl get clusterrolebinding castware-operator-manager-rolebinding -o yaml > "$OUTPUT_DIR/operator-manager-rolebinding.yaml" 2>&1 || echo "Manager rolebinding not found" > "$OUTPUT_DIR/operator-manager-rolebinding.yaml"

# Helm Release Information
echo "=== Collecting Helm Release Information ==="
helm list -n castai-agent > "$OUTPUT_DIR/helm-releases.txt" 2>&1
helm status castware-operator -n castai-agent > "$OUTPUT_DIR/helm-status.txt" 2>&1
helm get values castware-operator -n castai-agent > "$OUTPUT_DIR/helm-values.yaml" 2>&1
helm get manifest castware-operator -n castai-agent > "$OUTPUT_DIR/helm-manifest.yaml" 2>&1

# Events in castai-agent namespace
echo "=== Collecting Kubernetes Events ==="
kubectl get events -n castai-agent --sort-by='.lastTimestamp' > "$OUTPUT_DIR/events-castai-agent.txt" 2>&1

# castai-agent status
echo "=== Collecting castai-agent Information ==="
kubectl get pods -n castai-agent -l app.kubernetes.io/name=castai-agent -o wide > "$OUTPUT_DIR/agent-pods.txt" 2>&1
kubectl describe pods -n castai-agent -l app.kubernetes.io/name=castai-agent > "$OUTPUT_DIR/agent-pods-describe.txt" 2>&1
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-agent --tail=200 > "$OUTPUT_DIR/agent-logs.txt" 2>&1

# cluster-controller status
echo "=== Collecting cluster-controller Information ==="
kubectl get pods -n castai-agent -l app.kubernetes.io/name=castai-cluster-controller -o wide > "$OUTPUT_DIR/cluster-controller-pods.txt" 2>&1
kubectl describe pods -n castai-agent -l app.kubernetes.io/name=castai-cluster-controller > "$OUTPUT_DIR/cluster-controller-pods-describe.txt" 2>&1
kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-cluster-controller --tail=200 > "$OUTPUT_DIR/cluster-controller-logs.txt" 2>&1

# Network/Webhook Service Status
echo "=== Collecting Service Information ==="
kubectl get service -n castai-agent castware-operator-webhook-service -o yaml > "$OUTPUT_DIR/webhook-service.yaml" 2>&1 || echo "Webhook service not found"
kubectl get endpoints -n castai-agent castware-operator-webhook-service -o yaml > "$OUTPUT_DIR/webhook-endpoints.yaml" 2>&1 || echo "Webhook endpoints not found"

# Secrets (sanitized - only list, don't show values)
echo "=== Collecting Secrets List ==="
kubectl get secrets -n castai-agent -o custom-columns=NAME:.metadata.name,TYPE:.type,AGE:.metadata.creationTimestamp > "$OUTPUT_DIR/secrets-list.txt" 2>&1

# Namespace status
echo "=== Collecting Namespace Information ==="
kubectl get namespace castai-agent -o yaml > "$OUTPUT_DIR/namespace.yaml" 2>&1
kubectl describe namespace castai-agent > "$OUTPUT_DIR/namespace-describe.txt" 2>&1

# Cluster information
echo "=== Collecting Cluster Information ==="
kubectl version > "$OUTPUT_DIR/cluster-version.txt" 2>&1
kubectl get nodes -o wide > "$OUTPUT_DIR/cluster-nodes.txt" 2>&1

# Jobs (CRD upgrade job, etc.)
echo "=== Collecting Jobs Information ==="
kubectl get jobs -n castai-agent -o yaml > "$OUTPUT_DIR/jobs.yaml" 2>&1
kubectl describe jobs -n castai-agent > "$OUTPUT_DIR/jobs-describe.txt" 2>&1

# Compress the output
echo "=== Compressing output ==="
tar -czf "${OUTPUT_DIR}.tar.gz" "$OUTPUT_DIR"
rm -rf "$OUTPUT_DIR"

echo ""
echo "======================================"
echo "Data collection complete!"
echo "Please provide the file: ${OUTPUT_DIR}.tar.gz"
echo "======================================"
