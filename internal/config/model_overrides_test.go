package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Clearing an override must put the user's configured model back, not just
// drop the pin: a new session reads Config().Models, so an override left
// there keeps bleeding into it.
func TestClearModelOverridesRestoresConfiguredModel(t *testing.T) {
	t.Parallel()
	configured := SelectedModel{Model: "configured-large", Provider: "p"}
	s := NewTestStore(&Config{
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: configured,
		},
	})

	s.OverridePreferredModel(SelectedModelTypeLarge, SelectedModel{Model: "session-pinned", Provider: "p"})
	require.Equal(t, "session-pinned", s.Config().Models[SelectedModelTypeLarge].Model)

	s.ClearModelOverrides()

	require.Equal(t, configured, s.Config().Models[SelectedModelTypeLarge],
		"live config must return to the configured model")
	require.Empty(t, s.Overrides().Models, "the pin must be dropped too")
}

// A model type with nothing configured must not be left holding the
// override after a clear.
func TestClearModelOverridesRemovesUnconfiguredModel(t *testing.T) {
	t.Parallel()
	s := NewTestStore(&Config{})

	s.OverridePreferredModel(SelectedModelTypeSmall, SelectedModel{Model: "session-pinned", Provider: "p"})
	require.Equal(t, "session-pinned", s.Config().Models[SelectedModelTypeSmall].Model)

	s.ClearModelOverrides()

	_, ok := s.Config().Models[SelectedModelTypeSmall]
	require.False(t, ok, "an unconfigured model type must be removed, not left overridden")
}

// Two overrides in a row must still restore the original, not the first
// override.
func TestClearModelOverridesRestoresOriginalAfterRepeatedOverrides(t *testing.T) {
	t.Parallel()
	configured := SelectedModel{Model: "configured", Provider: "p"}
	s := NewTestStore(&Config{
		Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: configured},
	})

	s.OverridePreferredModel(SelectedModelTypeLarge, SelectedModel{Model: "first", Provider: "p"})
	s.OverridePreferredModel(SelectedModelTypeLarge, SelectedModel{Model: "second", Provider: "p"})
	s.ClearModelOverrides()

	require.Equal(t, configured, s.Config().Models[SelectedModelTypeLarge])
}
