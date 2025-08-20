package helm

import (
	"reflect"
	"strings"
	"testing"
)

func TestMergeValuesOverrides(t *testing.T) {
	t.Run("should add key when missing", func(t *testing.T) {
		values := map[string]interface{}{"a": 1}
		overrides := map[string]interface{}{"b": "new"}

		if err := mergeValuesOverrides(values, overrides); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := map[string]interface{}{"a": 1, "b": "new"}
		if !reflect.DeepEqual(values, want) {
			t.Fatalf("got %+v, want %+v", values, want)
		}
	})

	t.Run("should override simple types even if types mismatch", func(t *testing.T) {
		values := map[string]interface{}{"count": 1, "active": true}
		overrides := map[string]interface{}{"count": "2", "active": 0}

		if err := mergeValuesOverrides(values, overrides); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := map[string]interface{}{"count": "2", "active": 0}
		if !reflect.DeepEqual(values, want) {
			t.Fatalf("got %+v, want %+v", values, want)
		}
	})

	t.Run("should merge nested map[string]interface{} recursively", func(t *testing.T) {
		values := map[string]interface{}{
			"settings": map[string]interface{}{
				"db": map[string]interface{}{
					"host": "localhost",
					"port": 5432,
				},
				"feature": map[string]interface{}{"beta": false},
			},
		}
		overrides := map[string]interface{}{
			"settings": map[string]interface{}{
				"db":       map[string]interface{}{"port": 5433, "user": "admin"},
				"feature":  map[string]interface{}{"beta": true},
				"newGroup": map[string]interface{}{"enabled": true},
			},
		}

		if err := mergeValuesOverrides(values, overrides); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := map[string]interface{}{
			"settings": map[string]interface{}{
				"db":       map[string]interface{}{"host": "localhost", "port": 5433, "user": "admin"},
				"feature":  map[string]interface{}{"beta": true},
				"newGroup": map[string]interface{}{"enabled": true},
			},
		}
		if !reflect.DeepEqual(values, want) {
			t.Fatalf("got %+v, want %+v", values, want)
		}
	})

	t.Run("should error when overriding map with non-map", func(t *testing.T) {
		values := map[string]interface{}{
			"obj": map[string]interface{}{"x": 1},
		}
		overrides := map[string]interface{}{
			"obj": 42, // now disallowed
		}

		err := mergeValuesOverrides(values, overrides)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "obj") {
			t.Fatalf("error should mention 'obj', got: %v", err)
		}
	})

	t.Run("should allow overriding map with nil (treated as simple override)", func(t *testing.T) {
		values := map[string]interface{}{
			"obj": map[string]interface{}{"x": 1},
		}
		overrides := map[string]interface{}{
			"obj": nil, // allowed as a deletion/clear
		}

		if err := mergeValuesOverrides(values, overrides); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got, ok := values["obj"]; ok && got != nil {
			t.Fatalf("expected obj to be nil, got=%#v", got)
		}
	})

	t.Run("should allow overriding non-map with map", func(t *testing.T) {
		values := map[string]interface{}{"obj": 42}
		overrides := map[string]interface{}{"obj": map[string]interface{}{"x": 1}}

		if err := mergeValuesOverrides(values, overrides); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		got, ok := values["obj"].(map[string]interface{})
		if !ok || got["x"] != 1 {
			t.Fatalf("expected map override with x=1, got=%#v", values["obj"])
		}
	})

	t.Run("should return error when both sides are maps with different types (dst map[string]interface{}, src map[string]string)", func(t *testing.T) {
		values := map[string]interface{}{"labels": map[string]interface{}{"a": "1"}}
		overrides := map[string]interface{}{"labels": map[string]string{"b": "2"}}

		err := mergeValuesOverrides(values, overrides)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "labels") {
			t.Fatalf("error should mention 'labels', got: %v", err)
		}
	})

	t.Run("should return error when both sides are maps with different types (dst map[string]string, src map[string]interface{})", func(t *testing.T) {
		values := map[string]interface{}{"labels": map[string]string{"a": "1"}}
		overrides := map[string]interface{}{"labels": map[string]interface{}{"b": "2"}}

		err := mergeValuesOverrides(values, overrides)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "labels") {
			t.Fatalf("error should mention 'labels', got: %v", err)
		}
	})

	t.Run("should treat nil on either side as simple override (non-map -> nil)", func(t *testing.T) {
		values := map[string]interface{}{"a": nil, "b": 1, "c": "keep"}
		overrides := map[string]interface{}{"a": 2, "b": nil}

		if err := mergeValuesOverrides(values, overrides); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := map[string]interface{}{"a": 2, "b": nil, "c": "keep"}
		if !reflect.DeepEqual(values, want) {
			t.Fatalf("got %+v, want %+v", values, want)
		}
	})

	t.Run("should not disturb unrelated keys", func(t *testing.T) {
		values := map[string]interface{}{"left": "L", "right": "R"}
		overrides := map[string]interface{}{"left": 123}

		if err := mergeValuesOverrides(values, overrides); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := map[string]interface{}{"left": 123, "right": "R"}
		if !reflect.DeepEqual(values, want) {
			t.Fatalf("got %+v, want %+v", values, want)
		}
	})
}
