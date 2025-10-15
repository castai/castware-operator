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
