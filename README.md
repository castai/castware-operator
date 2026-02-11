# CAST AI Castware Operator

An operator that manages Castware components.

## How to run in a local environment
1. Generate an api key in console
2. Set the environment variable `API_KEY` with the generated key
3. Set the `API_URL` environment variable to the api url of the environment where you generated the api key
4. Run `make run-local`

Sample custom resources for cluster and component are generated in `local/samples`.

### Local environment cleanup
1. Run `make updeploy`

Ignore the error about the missing `castware-operator-controller-manager` deployment.

## How to run castware-operator in local kind cluster using local chart ./charts/castai-castware-operator

### Initial Setup

1. **Create Kind cluster**:
   ```bash
   kind create cluster --name castware-operator
   ```

2. **Set required environment variables**:
   ```bash
   export API_KEY="your-api-key-from-console"
   export API_URL="https://api.cast.ai"  # or your environment URL
   export API_KEY_BASE64=$(echo -n "$API_KEY" | base64)
   ```

3. **Install operator in Kind cluster**:
   ```bash
   ./local/install-local.sh
   ```

   This script will:
   - Build the operator binary (`make build`)
   - Build Docker image (`make docker-build`)
   - Load image into Kind cluster
   - Install Helm chart with local image
   - Create default Cluster and Component resources

### Development Workflow

After making code changes, quickly reload the operator without full reinstall:

```bash
./local/reload-operator.sh
```

This script will:
- Rebuild operator binary and Docker image
- Load new image into Kind cluster
- Restart the operator deployment
- Wait for rollout to complete

**Benefits:**
- ~30 seconds vs ~2 minutes for full reinstall
- Preserves existing CRs (Cluster and Components)
- No need to delete/recreate resources

### Monitoring

```bash
# Watch pods
kubectl get pods -n castai-agent -w

# View operator logs
kubectl logs -n castai-agent -l app.kubernetes.io/name=castware-operator -f

# Check Cluster CR status
kubectl get cluster -n castai-agent -o yaml

# Check Helm release status
helm status castware-operator -n castai-agent
```

### Cleanup

```bash
# Uninstall Helm release
helm uninstall castware-operator -n castai-agent

# Delete Kind cluster
kind delete cluster --name castware-operator
```

### How to use local helm registry

#### 1. Deploy ChartMuseum in the cluster

```bash
kubectl apply -f local/registry.yaml
kubectl wait --for=condition=ready pod -l app=chartmuseum -n registry --timeout=60s
```

#### 2. Port-forward to access ChartMuseum from your machine

```bash
kubectl port-forward -n registry svc/chartmuseum 5001:8080 &
```

#### 3. Add the local repo to Helm as castai-helm

```bash
helm repo add castai-helm http://localhost:5001/helm-charts
helm repo update
```

#### 4. Package and push local charts

```bash
cd charts

# Package the chart (update version in Chart.yaml first if needed)
helm package ./castai-castware-operator

# Push to ChartMuseum
curl --data-binary "@castware-operator-<version_number>.tgz" http://localhost:5001/helm-charts/api/charts

# Verify
curl -s http://localhost:5001/helm-charts/api/charts | jq 'keys'
```

#### 5. Mirror charts from public CAST AI repo (optional)

```bash
# Add public repo
helm repo add castai-public https://castai.github.io/helm-charts
helm repo update

# Pull and push to local registry
helm pull castai-public/castai-agent --version 0.127.0
helm pull castai/castai-spot-handler --version 0.29.0
curl --data-binary "@castai-agent-0.127.0.tgz" http://localhost:5001/helm-charts/api/charts
curl --data-binary "@castai-spot-handler-0.29.0.tgz" http://localhost:5001/helm-charts/api/charts		
```

#### 6. Test self-upgrade

1. Push at least two versions of `castware-operator` to the local registry
2. Install the operator with the older version:
   ```bash
   helm upgrade --install castware-operator castai-helm/castware-operator \
       --namespace castai-agent --create-namespace \
       --set defaultCluster.helmRepoURL="http://chartmuseum.registry.svc.cluster.local:8080/helm-charts" \
       --version "0.1.0"
       (...)
   ```
3. The operator will use the in-cluster URL (`chartmuseum.registry.svc.cluster.local:8080`) for self-upgrade
4. Trigger upgrade via CAST AI action or manually update the Cluster CR


### How to run e2e tests

#### Create kind cluster

```bash
kind create cluster --config test/e2e/kind-config.yaml --name castware-operator-e2e
```

#### Setup environment variables

```bash
export API_KEY="your-api-key-from-console"
export API_URL="https://api.dev-master.cast.ai"  # or your environment URL
export KIND_CLUSTER=castware-operator-e2e
```

#### Run tests
```bash
make e2e
```

#### Running specific test

```bash
go test -v -ginkgo.v -ginkgo.focus="$TEST_PATTERN" -timeout 30m
```
e.g.

```bash
go test -v -ginkgo.v -ginkgo.focus="should run successfully" -timeout 30m
```