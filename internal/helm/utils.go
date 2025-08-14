package helm

import (
	"fmt"
	"reflect"
)

// mergeValuesOverrides merges `overrides` into `values` with rules:
//   - If both sides are map[string]interface{}, merge recursively.
//   - If key does not exist in values, set it.
//   - If either side is nil, simple override.
//   - For non-map (simple) types, always override (even if types differ).
//   - If BOTH sides are maps but of DIFFERENT map types (e.g., map[string]string vs map[string]interface{}), return error.
//   - If values is a map and overrides is a non-map (non-nil), return error.
//     (Overriding a map with a non-map is disallowed; overriding with nil is allowed.)
//   - If both sides are maps of the SAME non-MSI type, simple override (no recursion).
func mergeValuesOverrides(values map[string]interface{}, overrides map[string]interface{}) error {
	return mergeRecursive(values, overrides, "")
}

func mergeRecursive(dst, src map[string]interface{}, path string) error {
	for k, v := range src {
		keyPath := k
		if path != "" {
			keyPath = path + "." + k
		}

		cur, exists := dst[k]
		if !exists {
			dst[k] = v
			continue
		}

		// nil on either side -> simple override
		if cur == nil || v == nil {
			dst[k] = v
			continue
		}

		curRV := reflect.ValueOf(cur)
		vRV := reflect.ValueOf(v)

		// If current is a map but override is a non-map (and non-nil) -> error
		if curRV.Kind() == reflect.Map && vRV.Kind() != reflect.Map {
			return fmt.Errorf("cannot override map with non-map at %q: have %s, override is %T", keyPath, curRV.Type(), v)
		}

		// If both are maps, handle map rules
		if curRV.Kind() == reflect.Map && vRV.Kind() == reflect.Map {
			// Recursively merge only when both are map[string]interface{}
			if curMSI, ok1 := cur.(map[string]interface{}); ok1 {
				if vMSI, ok2 := v.(map[string]interface{}); ok2 {
					if err := mergeRecursive(curMSI, vMSI, keyPath); err != nil {
						return err
					}
					continue
				}
			}

			// Different map types => error
			if curRV.Type() != vRV.Type() {
				return fmt.Errorf("map type mismatch at %q: have %s, override is %s", keyPath, curRV.Type(), vRV.Type())
			}

			// Same non-MSI map type => simple override (no recursion)
			dst[k] = v
			continue
		}

		// If dst is non-map and src is map, allow override
		// If both are non-maps, always override (even if types differ)
		dst[k] = v
	}

	return nil
}
