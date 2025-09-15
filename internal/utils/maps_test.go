package utils

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnflattenMap(t *testing.T) {
	t.Parallel()

	t.Run("should return empty map for empty input", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		out, err := UnflattenMap(map[string]string{})
		r.NoError(err)
		r.Equal(map[string]interface{}{}, out)
	})

	t.Run("should handle single-level key", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		in := map[string]string{
			"a": "1",
		}
		want := map[string]interface{}{
			"a": "1",
		}

		out, err := UnflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})

	t.Run("should handle nested keys", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		in := map[string]string{
			"a.b": "1",
			"a.c": "2",
		}
		want := map[string]interface{}{
			"a": map[string]interface{}{
				"b": "1",
				"c": "2",
			},
		}

		out, err := UnflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})

	t.Run("should handle deep nesting", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		in := map[string]string{
			"a.b.c.d": "x",
		}
		want := map[string]interface{}{
			"a": map[string]interface{}{
				"b": map[string]interface{}{
					"c": map[string]interface{}{
						"d": "x",
					},
				},
			},
		}

		out, err := UnflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})

	t.Run("should merge siblings across different branches", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		in := map[string]string{
			"a.b":   "1",
			"a.d.e": "2",
			"z":     "top",
		}
		want := map[string]interface{}{
			"a": map[string]interface{}{
				"b": "1",
				"d": map[string]interface{}{
					"e": "2",
				},
			},
			"z": "top",
		}

		out, err := UnflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})

	t.Run("should be order independent", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		in1 := map[string]string{
			"a.b": "1",
			"a.c": "2",
		}
		in2 := map[string]string{
			"a.c": "2",
			"a.b": "1",
		}
		want := map[string]interface{}{
			"a": map[string]interface{}{
				"b": "1",
				"c": "2",
			},
		}

		out1, err1 := UnflattenMap(in1)
		out2, err2 := UnflattenMap(in2)
		r.NoError(err1)
		r.NoError(err2)
		r.Equal(want, out1)
		r.Equal(want, out2)
	})

	t.Run("should error when a path conflicts with a leaf value", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		in := map[string]string{
			"a":   "leaf",
			"a.b": "child",
		}

		out, err := UnflattenMap(in)
		r.Nil(out)
		r.EqualError(err, "invalid map")
	})

	t.Run("should error when intermediate is not a map", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		in := map[string]string{
			"a.b":   "1",
			"a.b.c": "2", // tries to descend into a string at a.b
		}

		out, err := UnflattenMap(in)
		r.Nil(out)
		r.EqualError(err, "invalid map")
	})

	t.Run("should allow separate top-level keys", func(t *testing.T) {
		t.Parallel()
		r := require.New(t)

		in := map[string]string{
			"foo":     "bar",
			"alpha.x": "1",
			"alpha.y": "2",
			"beta.z":  "3",
		}
		want := map[string]interface{}{
			"foo": "bar",
			"alpha": map[string]interface{}{
				"x": "1",
				"y": "2",
			},
			"beta": map[string]interface{}{
				"z": "3",
			},
		}

		out, err := UnflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})
}

func TestMergeMaps(t *testing.T) {
	t.Run("should add key when missing", func(t *testing.T) {
		values := map[string]interface{}{"a": 1}
		overrides := map[string]interface{}{"b": "new"}

		if err := MergeMaps(values, overrides); err != nil {
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

		if err := MergeMaps(values, overrides); err != nil {
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

		if err := MergeMaps(values, overrides); err != nil {
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

		err := MergeMaps(values, overrides)
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

		if err := MergeMaps(values, overrides); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got, ok := values["obj"]; ok && got != nil {
			t.Fatalf("expected obj to be nil, got=%#v", got)
		}
	})

	t.Run("should allow overriding non-map with map", func(t *testing.T) {
		values := map[string]interface{}{"obj": 42}
		overrides := map[string]interface{}{"obj": map[string]interface{}{"x": 1}}

		if err := MergeMaps(values, overrides); err != nil {
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

		err := MergeMaps(values, overrides)
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

		err := MergeMaps(values, overrides)
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

		if err := MergeMaps(values, overrides); err != nil {
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

		if err := MergeMaps(values, overrides); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := map[string]interface{}{"left": 123, "right": "R"}
		if !reflect.DeepEqual(values, want) {
			t.Fatalf("got %+v, want %+v", values, want)
		}
	})
}
