package common

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalWithNumberRejectsTrailingJSON(t *testing.T) {
	var value map[string]any

	err := UnmarshalWithNumber([]byte(`{"id":9007199254740993} {"ignored":true}`), &value)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing data")
}

func TestUnmarshalWithNumberPreservesLargeInteger(t *testing.T) {
	var value map[string]any

	require.NoError(t, UnmarshalWithNumber([]byte(`{"id":9007199254740993}`), &value))
	require.IsType(t, json.Number(""), value["id"])
	assert.Equal(t, json.Number("9007199254740993"), value["id"])
}

func TestJsonRawMessageToString(t *testing.T) {
	tests := []struct {
		name string
		data json.RawMessage
		want string
	}{
		{
			name: "object",
			data: json.RawMessage(`{"city":"Paris","days":0,"strict":false}`),
			want: `{"city":"Paris","days":0,"strict":false}`,
		},
		{
			name: "string",
			data: json.RawMessage(`"{\"city\":\"Paris\",\"days\":0,\"strict\":false}"`),
			want: `{"city":"Paris","days":0,"strict":false}`,
		},
		{
			name: "null",
			data: json.RawMessage(`null`),
			want: "",
		},
		{
			name: "empty",
			data: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, JsonRawMessageToString(tt.data))
		})
	}
}
