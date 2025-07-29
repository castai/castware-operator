#!/bin/bash

# Define variables
SECRET_NAME="castware-operator-certs"
NAMESPACE="castai-agent"
OUTPUT_DIR="certs"

# Create output directory if it doesn't exist
mkdir -p "$OUTPUT_DIR"

# Get the secret data in JSON format
SECRET_DATA=$(kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" -o json)

# Extract all key names from the secret
KEYS=$(echo "$SECRET_DATA" | jq -r '.data | keys[]')

# Loop through each key and decode the base64 value
for KEY in $KEYS; do
  VALUE=$(echo "$SECRET_DATA" | jq -r ".data[\"$KEY\"]" | base64 -d)
  echo "$VALUE" > "$OUTPUT_DIR/$KEY"
  echo "Written: $OUTPUT_DIR/$KEY"
done
