package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnflattenMap(t *testing.T) {

	t.Run("should return empty map for empty input", func(t *testing.T) {
		r := require.New(t)

		out, err := unflattenMap(map[string]string{})
		r.NoError(err)
		r.Equal(map[string]interface{}{}, out)
	})

	t.Run("should handle single-level key", func(t *testing.T) {
		r := require.New(t)

		in := map[string]string{
			"a": "1",
		}
		want := map[string]interface{}{
			"a": "1",
		}

		out, err := unflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})

	t.Run("should handle nested keys", func(t *testing.T) {
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

		out, err := unflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})

	t.Run("should handle deep nesting", func(t *testing.T) {
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

		out, err := unflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})

	t.Run("should merge siblings across different branches", func(t *testing.T) {
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

		out, err := unflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})

	t.Run("should be order independent", func(t *testing.T) {
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

		out1, err1 := unflattenMap(in1)
		out2, err2 := unflattenMap(in2)
		r.NoError(err1)
		r.NoError(err2)
		r.Equal(want, out1)
		r.Equal(want, out2)
	})

	t.Run("should error when a path conflicts with a leaf value", func(t *testing.T) {
		r := require.New(t)

		in := map[string]string{
			"a":   "leaf",
			"a.b": "child",
		}

		out, err := unflattenMap(in)
		r.Nil(out)
		r.EqualError(err, "invalid map")
	})

	t.Run("should error when intermediate is not a map", func(t *testing.T) {
		r := require.New(t)

		in := map[string]string{
			"a.b":   "1",
			"a.b.c": "2", // tries to descend into a string at a.b
		}

		out, err := unflattenMap(in)
		r.Nil(out)
		r.EqualError(err, "invalid map")
	})

	t.Run("should allow separate top-level keys", func(t *testing.T) {
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

		out, err := unflattenMap(in)
		r.NoError(err)
		r.Equal(want, out)
	})
}
