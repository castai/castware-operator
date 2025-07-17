
CERT_MANAGER_CRD="certificates.cert-manager.io"
CERT_MANAGER_RELEASE_URL="https://github.com/cert-manager/cert-manager/releases/download/v1.18.0/cert-manager.yaml"


if kubectl get crd "${CERT_MANAGER_CRD}" >/dev/null 2>&1; then
  echo "✅ cert-manager crd '${CERT_MANAGER_CRD}' is already installed."
else
  echo "⚠️  cert-manager crd '${CERT_MANAGER_CRD}' not found. Installing cert-manager v1.18.0..."
  kubectl apply -f "${CERT_MANAGER_RELEASE_URL}"
  echo "✅ cert-manager installation manifest applied."
fi

# Check if API_KEY is unset or empty
if [ -z "${API_KEY}" ]; then
  echo "Error: API_KEY environment variable is not set." >&2
  exit 1
fi

if [ -z "${API_URL}" ]; then
  API_URL="https://api.dev-master.cast.ai"
  echo "API_URL was not set. Defaulting to ${API_URL}"
fi

if [ -z "${PROVIDER}" ]; then
  # Set your desired default here:
  PROVIDER="gke"
  echo "PROVIDER was not set. Defaulting to ${PROVIDER}"
fi

echo "Installing Castware operator"

helm install castware-operator \
--namespace castai-agent --create-namespace \
--set image.tag=latest \
--set apiKeySecret.apiKey="${API_KEY}" \
--set defaultCluster.provider="${PROVIDER}" \
--set defaultCluster.api.apiUrl="${API_URL}" \
--set image.pullPolicy=Always  \
--wait \
castai-helm/castware-operator