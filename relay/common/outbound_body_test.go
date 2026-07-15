package common

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorageMayContainSensitiveLocationDataFindsMarkerAcrossChunks(t *testing.T) {
	prefix := bytes.Repeat([]byte{'x'}, 32*1024-5)
	storage := &diskLikeBodyStorage{Reader: bytes.NewReader(append(prefix, []byte(`"user_location"`)...))}
	_, err := storage.Seek(17, io.SeekStart)
	require.NoError(t, err)

	found, err := storageMayContainSensitiveLocationData(storage)

	require.NoError(t, err)
	require.True(t, found)
	offset, err := storage.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	require.Equal(t, int64(17), offset)
}

type diskLikeBodyStorage struct {
	*bytes.Reader
}

func (s *diskLikeBodyStorage) Bytes() ([]byte, error) {
	return nil, nil
}

func (s *diskLikeBodyStorage) Size() int64 {
	return s.Reader.Size()
}

func (s *diskLikeBodyStorage) IsDisk() bool {
	return true
}

func (s *diskLikeBodyStorage) Close() error {
	return nil
}
