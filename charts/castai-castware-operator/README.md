# castware-operator

An operator that manages Castware components. Components are installed as Helm
releases driven by `Cluster` and `Component` custom resources.

## Installation

```bash
helm repo add castai-helm https://castai.github.io/helm-charts
helm repo update

helm upgrade --install castware-operator castai-helm/castware-operator \
  --namespace castai-agent --create-namespace
```

## Image registry

CAST AI images are migrating to GitHub Container Registry — the operator image
defaults to `ghcr.io/castai/images/castware-operator`. The same image is also
published to Google Artifact Registry under an identical path
(`us-docker.pkg.dev/castai-hub/library/castware-operator`), and both registries
are fully supported.

If your clusters cannot pull from `ghcr.io` (e.g. a registry allowlist), install
the operator with the GAR image instead:

```bash
helm upgrade --install castware-operator castai-helm/castware-operator \
  --set image.repository=us-docker.pkg.dev/castai-hub/library/castware-operator
```

Components managed by the operator take Helm values through
`Component.spec.values`, so their registries are overridden the same way as a
direct Helm install of each chart. For the umbrella component, the umbrella
chart's ready-made
[gar-values.yaml](https://github.com/castai/helm-charts/blob/main/charts/castai-umbrella/gar-values.yaml)
content applies verbatim via `defaultComponents.umbrella.overrides` - a single
operator install covers both the operator image and the umbrella component
(requires [yq](https://github.com/mikefarah/yq)):

```bash
curl -sO https://raw.githubusercontent.com/castai/helm-charts/main/charts/castai-umbrella/gar-values.yaml
yq '{"defaultComponents": {"umbrella": {"enabled": true, "overrides": .}}}' gar-values.yaml > operator-gar.yaml

helm upgrade --install castware-operator castai-helm/castware-operator \
  --namespace castai-agent --create-namespace \
  --set image.repository=us-docker.pkg.dev/castai-hub/library/castware-operator \
  --set apiKeySecret.apiKey=<KEY> \
  --set extendedPermissions=true \
  -f operator-gar.yaml
```
