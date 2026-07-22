package common

import (
	"bytes"
	"io"
	"strings"
	"testing"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

type trackingBodyStorage struct {
	*bytes.Reader
	data       []byte
	bytesCalls int
}

func newTrackingBodyStorage(data []byte) *trackingBodyStorage {
	return &trackingBodyStorage{Reader: bytes.NewReader(data), data: data}
}

func (s *trackingBodyStorage) Close() error { return nil }

func (s *trackingBodyStorage) Bytes() ([]byte, error) {
	s.bytesCalls++
	return s.data, nil
}

func (s *trackingBodyStorage) Size() int64 { return int64(len(s.data)) }

func (s *trackingBodyStorage) IsDisk() bool { return false }

func TestNewPrivacyFilteredPassThroughJSONBodyFiltersNormalizedSensitiveKeys(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeStrip))
	storage, err := rootcommon.CreateBodyStorage([]byte(`{"metadata":{"TRUE-CLIENT-IP":"203.0.113.10","keep":"value"}}`))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })

	body, _, closer, err := NewPrivacyFilteredPassThroughJSONBody(storage)

	require.NoError(t, err)
	if closer != nil {
		t.Cleanup(func() { require.NoError(t, closer.Close()) })
	}
	output, err := io.ReadAll(body)
	require.NoError(t, err)
	require.JSONEq(t, `{"metadata":{"keep":"value"}}`, string(output))
}

func TestPreparedOutboundJSONSharesPrivacyFilteredBytesWithTransport(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeStrip))

	prepared, err := PrepareOutboundJSON([]byte(
		`{"model":"gpt-5","metadata":{"city":"Shanghai","keep":"value"}}`,
	))
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"gpt-5","metadata":{"keep":"value"}}`, string(prepared.Bytes()))

	body, size, closer, err := prepared.NewBody()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closer.Close()) })
	transportBytes, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, int64(len(prepared.Bytes())), size)
	require.Equal(t, prepared.Bytes(), transportBytes)
}

func TestPreparedOutboundJSONCannotBeMutatedThroughInputOrInspectionBytes(t *testing.T) {
	restoreLocationSettings(t)
	require.NoError(t, rootcommon.SetUpstreamLocationMode(rootcommon.UpstreamLocationModeClient))

	input := []byte(`{"model":"gpt-5","input":"hello"}`)
	prepared, err := PrepareOutboundJSON(input)
	require.NoError(t, err)

	input[2] = 'X'
	inspection := prepared.Bytes()
	inspection[2] = 'Y'

	body, _, closer, err := prepared.NewBody()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closer.Close()) })
	transportBytes, err := io.ReadAll(body)
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"gpt-5","input":"hello"}`, string(transportBytes))
}

func TestNewPrivacyFilteredPassThroughJSONBodySkipsMaterializingLargeSafeRequest(t *testing.T) {
	restoreLocationSettings(t)
	storage := newTrackingBodyStorage([]byte(`{"model":"gpt-5","metadata":{"request_kind":"turn"},"input":"` + strings.Repeat("x", 2*1024*1024) + `"}`))

	body, size, closer, err := NewPrivacyFilteredPassThroughJSONBody(storage)

	require.NoError(t, err)
	require.Nil(t, closer)
	require.Equal(t, int64(len(storage.data)), size)
	require.Zero(t, storage.bytesCalls)
	output, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, storage.data, output)
}

func TestStorageMayContainLocationPrivacyDataFindsKeyAcrossBufferBoundary(t *testing.T) {
	prefix := `{"input":"` + strings.Repeat("x", 32*1024-12) + `","metadata":{"remote_addr":"203.0.113.1"}}`
	storage := newTrackingBodyStorage([]byte(prefix))

	found, err := storageMayContainLocationPrivacyData(storage)

	require.NoError(t, err)
	require.True(t, found)
}

func TestStorageMayContainLocationPrivacyDataDecodesEscapedKey(t *testing.T) {
	storage := newTrackingBodyStorage([]byte(`{"metadata":{"\u0052EMOTE-ADDR":"203.0.113.1"}}`))

	found, err := storageMayContainLocationPrivacyData(storage)

	require.NoError(t, err)
	require.True(t, found)
}

func TestStorageMayContainLocationPrivacyDataRestoresOffset(t *testing.T) {
	storage := newTrackingBodyStorage([]byte(`{"metadata":{"remote_addr":"203.0.113.1"}}`))
	_, err := storage.Seek(7, io.SeekStart)
	require.NoError(t, err)

	found, err := storageMayContainLocationPrivacyData(storage)

	require.NoError(t, err)
	require.True(t, found)
	offset, err := storage.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	require.Equal(t, int64(7), offset)
}
