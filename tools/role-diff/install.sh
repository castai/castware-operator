#!/bin/bash
set -e

echo "Building role-diff tool..."
go build -o role-diff .

echo "✓ Build successful!"
echo ""
echo "Usage: ./role-diff <file1.yaml> <file2.yaml>"
echo ""
echo "Example:"
echo "  ./role-diff ../../config/rbac/role.yaml ../../config/rbac/phase2-role.yaml"
