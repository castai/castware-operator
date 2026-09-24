package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrdNamesFromManifest verifies the umbrella release CRD extraction:
// only CustomResourceDefinition documents contribute names, taken from
// metadata.name structurally — nested name: fields (ownerReferences,
// spec.names) must never be captured.
func TestCrdNamesFromManifest(t *testing.T) {
	manifest := `---
# Source: cast/templates/pod-mutations.yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: podmutations.pod-mutations.cast.ai
  annotations:
    helm.sh/hashed-name: irrelevant
  ownerReferences:
  - apiVersion: v1
    name: some-owner
spec:
  group: pod-mutations.cast.ai
  names:
    kind: PodMutation
    listKind: PodMutationList
    plural: podmutations
---
# Source: cast/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: castai-agent
spec:
  template:
    metadata:
      name: agent-pod
---
# Source: cast/templates/metrics.yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: recommendations.autoscaling.cast.ai
spec:
  names:
    plural: recommendations
`

	names, err := crdNamesFromManifest(manifest)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"podmutations.pod-mutations.cast.ai",
		"recommendations.autoscaling.cast.ai",
	}, names)
}

// TestFindComponentCondition verifies the exact typed matching of component
// status conditions: the type and reason must belong to the same condition,
// regardless of the order the fields were serialized in.
func TestFindComponentCondition(t *testing.T) {
	// Reason serialized before type, plus another condition sharing the type.
	conditions := []componentCondition{
		{Type: "Available", Status: "True", Reason: "Installed"},
		{Type: "UmbrellaConflict", Status: "True", Reason: "UmbrellaReleasePresent"},
		{Type: "UmbrellaConflict", Status: "False", Reason: "UmbrellaReleaseNotPresent"},
	}

	found := findComponentCondition(conditions, "UmbrellaConflict", "UmbrellaReleasePresent")
	require.NotNil(t, found)
	assert.Equal(t, "True", found.Status)

	found = findComponentCondition(conditions, "UmbrellaConflict", "")
	require.NotNil(t, found)
	assert.Equal(t, "UmbrellaReleasePresent", found.Reason) // first of the type, reason ignored

	assert.Nil(t, findComponentCondition(conditions, "UmbrellaConflict", "IndividualReleasesPresent"))
	assert.Nil(t, findComponentCondition(conditions, "Progressing", "UmbrellaReleasePresent"))
	assert.Nil(t, findComponentCondition(nil, "UmbrellaConflict", "UmbrellaReleasePresent"))
}
