# Role Diff Tool

A command-line tool to compare two Kubernetes Role or ClusterRole YAML files and display the differences.

## Usage

```bash
# Build the tool
cd tools/role-diff
go build -o role-diff

# Compare two role files
./role-diff path/to/role1.yaml path/to/role2.yaml
```

## Features

- Compares PolicyRules between two Role or ClusterRole resources
- Shows a detailed diff using go-cmp
- Provides a detailed analysis showing:
  - Rules only in the first file
  - Rules only in the second file
  - Permissions added or removed
- Sorts rules and permissions for consistent comparison

## Example

```bash
./role-diff ../../config/rbac/role.yaml ../../charts/castai-castware-operator/templates/rbac.yaml
```

## Output

The tool provides three types of output:

1. **Summary**: Shows which files are being compared and their resource names
2. **Diff**: Shows the raw difference using go-cmp format
3. **Detailed Analysis**:
   - Lists rules unique to each file
   - Shows permissions added or removed in a readable format (e.g., `get:core:pods`)
