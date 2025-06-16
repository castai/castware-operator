#!/bin/bash

set -euo pipefail

CERT_DIR="certs"
CERT_CRT="$CERT_DIR/tls.crt"
CERT_KEY="$CERT_DIR/tls.key"
KIND_CLUSTER_NAME="castware-operator"
WEBHOOK_HOST="host.docker.internal"

# Check and generate certs if not exist
if [[ ! -f "$CERT_CRT" || ! -f "$CERT_KEY" ]]; then
  echo "Certificates not found, generating with mkcert..."
  mkdir -p "$CERT_DIR"
  mkcert -cert-file "$CERT_CRT" -key-file "$CERT_KEY" "$WEBHOOK_HOST"
else
  echo "Certificates already exist."
fi

# Set environment variables
export WEBHOOK_CERT_DIR="$(pwd)/$CERT_DIR"
export WEBHOOK_PORT=9443

# Check if kind cluster exists, create if not
if ! kind get clusters | grep -q "^$KIND_CLUSTER_NAME\$"; then
  echo "Kind cluster '$KIND_CLUSTER_NAME' not found, creating..."
  kind create cluster --name "$KIND_CLUSTER_NAME"
else
  echo "Kind cluster '$KIND_CLUSTER_NAME' already exists."
fi

kubectl config use-context "kind-$KIND_CLUSTER_NAME"
kubectl apply -f webhooks.yaml
cd ..
make install
make run
