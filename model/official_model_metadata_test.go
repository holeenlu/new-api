package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetOfficialModelMetadata(t *testing.T) {
	tests := []struct {
		modelName string
		window    int
		maxWindow int
		found     bool
	}{
		{modelName: "claude-fable-5", window: 1_000_000, found: true},
		{modelName: "claude-opus-4-8", window: 1_000_000, found: true},
		{modelName: "claude-sonnet-4-20250514", window: 200_000, found: true},
		{modelName: "claude-opus-4-6-high", found: false},
		{modelName: "gemini-2.5-pro", window: 1_048_576, found: true},
		{modelName: "gemini-2.5-flash-image", window: 65_536, found: true},
		{modelName: "gemini-3.1-pro-preview", window: 1_048_576, found: true},
		{modelName: "gpt-5.4", window: 1_050_000, found: true},
		{modelName: "gpt-5.5", window: 272_000, maxWindow: 1_000_000, found: true},
		{modelName: "gpt-5.6-sol", window: 272_000, maxWindow: 1_000_000, found: true},
		{modelName: "gpt-5.6-terra", window: 272_000, maxWindow: 1_000_000, found: true},
		{modelName: "gpt-5.6-luna", window: 272_000, maxWindow: 1_000_000, found: true},
		{modelName: "gpt-4.1-mini", window: 1_047_576, found: true},
		{modelName: "gpt-4.1-mini-2025-04-14", window: 1_047_576, found: true},
		{modelName: "gpt-4o", window: 128_000, found: true},
		{modelName: "gpt-3.5-turbo-0613", window: 4_096, found: true},
		{modelName: "custom-model", found: false},
	}

	for _, test := range tests {
		t.Run(test.modelName, func(t *testing.T) {
			metadata, found := GetOfficialModelMetadata(test.modelName)
			require.Equal(t, test.found, found)
			if found {
				require.Equal(t, test.window, metadata.ContextWindow)
				maxWindow := test.maxWindow
				if maxWindow == 0 {
					maxWindow = test.window
				}
				require.Equal(t, maxWindow, metadata.MaxContextWindow)
				require.True(t, metadata.Valid())
			}
		})
	}
}
