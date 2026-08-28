package utils

import (
	"encoding/json"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// UnmarshalJSON unmarshals an apiextensionsv1.JSON into a map[string]any.
// Returns an empty (non-nil) map if j is nil or empty so callers can always
// index into the result safely.
func UnmarshalJSON(j *apiextensionsv1.JSON) (map[string]any, error) {
	out := map[string]any{}
	if j == nil || len(j.Raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(j.Raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
