helm install --namespace castai-agent castai-agent \
--set apiKey="$API_KEY" \
--set additionalEnv.GKE_CLUSTER_NAME=castware-operator-test \
--set additionalEnv.GKE_LOCATION=local \
--set additionalEnv.GKE_PROJECT_ID=local-test \
--set additionalEnv.GKE_REGION=local1 \
--set provider=gke \
--set apiURL=https://api.dev-master.cast.ai \
--set createNamespace=false \
castai-helm/castai-agent