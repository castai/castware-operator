package utils

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMarshalReleaseConfig(t *testing.T) {
	t.Parallel()

	t.Run("json-safe config marshals directly", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		raw, err := MarshalReleaseConfig(map[string]any{
			"str":   "v",
			"int":   1,
			"float": 1.5,
			"bool":  true,
			"nested": map[string]any{
				"list": []any{"a", 2},
			},
		})
		r.NoError(err)
		var out map[string]any
		r.NoError(json.Unmarshal(raw, &out))
		r.Equal("v", out["str"])
		r.Equal(map[string]any{"list": []any{"a", float64(2)}}, out["nested"])
	})

	t.Run("empty and nil configs return nil", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		raw, err := MarshalReleaseConfig(nil)
		r.NoError(err)
		r.Nil(raw)
		raw, err = MarshalReleaseConfig(map[string]any{})
		r.NoError(err)
		r.Nil(raw)
	})

	t.Run("yaml-decoded interface-keyed maps are normalized to string keys", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		// The shape gopkg.in/yaml.v2 decoding produces for nested YAML maps:
		// map[interface{}]interface{} under a map[string]interface{} config.
		raw, err := MarshalReleaseConfig(map[string]any{
			"additionalEnv": map[any]any{
				"KEY": "value",
				42:    "int-key",
			},
			"list": []any{
				map[any]any{"inner": true},
			},
		})
		r.NoError(err)
		var out map[string]any
		r.NoError(json.Unmarshal(raw, &out))
		r.Equal(map[string]any{
			"KEY": "value",
			"42":  "int-key",
		}, out["additionalEnv"])
		r.Equal([]any{map[string]any{"inner": true}}, out["list"])
	})

	t.Run("values that cannot be represented in JSON error", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		_, err := MarshalReleaseConfig(map[string]any{
			"bad": make(chan int),
		})
		r.Error(err)
	})

	t.Run("normalization does not mutate the input", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)
		in := map[string]any{
			"nested": map[any]any{"k": "v"},
		}
		_, err := MarshalReleaseConfig(in)
		r.NoError(err)
		// The interface-keyed map is still interface-keyed (a copy was
		// normalized, not the original).
		_, ok := in["nested"].(map[any]any)
		r.True(ok)
	})
}
