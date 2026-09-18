package utils

import (
	"encoding/json"
	"fmt"

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

// MarshalReleaseConfig marshals a helm release's user-supplied config into
// raw JSON, tolerating YAML-decoded value types that json.Marshal rejects.
//
// Release configs read back from helm storage are JSON-decoded and marshal
// directly (the fast path). Configs written through the SDK or by third-party
// tooling can, however, carry YAML-decoded values — most notably nested
// map[interface{}]interface{} — which json.Marshal rejects. Such values are
// normalized (interface-keyed maps re-keyed as strings, recursively) and the
// normalized form marshaled instead.
//
// Returns nil (not an error) for an empty config. An error is returned only
// for values that cannot be represented in JSON even after normalization
// (e.g. channels or functions); callers decide how to degrade.
func MarshalReleaseConfig(config map[string]any) (json.RawMessage, error) {
	if len(config) == 0 {
		return nil, nil
	}
	if raw, err := json.Marshal(config); err == nil {
		return raw, nil
	}
	normalized, err := normalizeJSONValue(config)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// normalizeJSONValue rewrites YAML-decoded value types into their
// JSON-marshalable equivalents: map[interface{}]interface{} (and other
// non-string-keyed maps) become map[string]interface{} keyed by the string
// form of their keys, recursively. Everything else passes through
// unchanged; values json.Marshal still rejects after normalization are
// surfaced as an error naming the offending value.
func normalizeJSONValue(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			nv, err := normalizeJSONValue(vv)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", k, err)
			}
			out[k] = nv
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			ks := fmt.Sprint(k)
			nv, err := normalizeJSONValue(vv)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", ks, err)
			}
			out[ks] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			nv, err := normalizeJSONValue(vv)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = nv
		}
		return out, nil
	default:
		// json.Marshal reports the unsupported value itself; the error message
		// already names its type.
		return v, nil
	}
}
