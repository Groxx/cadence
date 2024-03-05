package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestArgs(t *testing.T) {
	t.Run("basics", func(t *testing.T) {
		keyed, positional, first, outfile, err := args("arg")
		assert.NoError(t, err)
		assert.Empty(t, keyed)
		assert.Equal(t, []string{"arg"}, positional)
		assert.Equal(t, "arg", first)
		assert.Zero(t, outfile)
	})
	t.Run("mixed", func(t *testing.T) {
		keyed, positional, first, outfile, err := args("arg", "-key", "value", "trailing", "-out", "outfile.txt", "supertrailing")
		assert.NoError(t, err)
		assert.Equal(t, map[string]string{"key": "value"}, keyed)
		assert.Equal(t, []string{"arg", "trailing", "supertrailing"}, positional)
		assert.Equal(t, "arg", first)
		assert.Equal(t, "outfile.txt", outfile)
	})
	t.Run("first keyed is first", func(t *testing.T) {
		keyed, positional, first, outfile, err := args("-key", "value", "trailing")
		assert.NoError(t, err)
		assert.Equal(t, map[string]string{"key": "value"}, keyed)
		assert.Equal(t, []string{"trailing"}, positional)
		assert.Equal(t, "value", first)
		assert.Empty(t, outfile)
	})
}

func TestBasics(t *testing.T) {
	assert.Equal(t, "Snake_Case", camel("snake__case"))
	assert.Equal(t, "camel__case", snake("Camel_Case"))
}

func FuzzSafety(f *testing.F) {
	f.Add("CamelCase")
	f.Add("snake_case")
	f.Add("mixedCase")
	f.Fuzz(func(t *testing.T, s string) {
		// shouldn't panic regardless of input
		snake(s)
		camel(s)
	})
}

func FuzzRoundTrip(f *testing.F) {
	f.Add("CamelCase")
	f.Add("snake_case")
	f.Add("mixedCase")
	f.Fuzz(func(t *testing.T, s string) {
		// case folding is host/lang-dependent outside ascii, and "_" will disappear
		// when camel-casing if the char is not uppercase-able.
		//
		// that's fine for fuzzing, just ignore them.
		for _, r := range s {
			if !(('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || (r == '_')) {
				t.Skip("outside simple filename/type chars, left undefined")
			}
		}
		if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
			t.Skip("ignoring non-infixed _, left undefined")
		}

		assert.Equal(t, snake(s), snake(camel(snake(s))), "snake->camel->snake failed with %q", s)
		assert.Equal(t, camel(s), camel(snake(camel(s))), "camel->snake->camel failed with %q", s)
	})
}
