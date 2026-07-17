package common

import (
	"bufio"
	"bytes"
	"io"

	"github.com/QuantumNous/new-api/common"
)

// NewOutboundJSONBody wraps the already-marshaled upstream request body into a
// BodyStorage. When disk cache is enabled and the payload exceeds the configured
// threshold, the data is written to a temp file and the original []byte can be
// GC'd, significantly reducing the heap residency while waiting for the
// upstream provider to respond (the dominant cost for large base64 payloads).
//
// In memory mode the underlying memoryStorage reuses the same backing array,
// so this is equivalent to bytes.NewReader(data) in terms of memory usage.
//
// The caller MUST invoke closer.Close() once the upstream call has finished
// (typically via defer) to release the disk file / memory accounting.
//
// The returned reader is wrapped with common.ReaderOnly to prevent the HTTP
// transport from prematurely closing the underlying BodyStorage. The returned
// size is meant to be propagated to http.Request.ContentLength because the
// type-erased io.Reader prevents net/http from auto-detecting it.
func NewOutboundJSONBody(data []byte, channelUsesProxy ...bool) (body io.Reader, size int64, closer io.Closer, err error) {
	mayContainLocationData, err := readerMayContainLocationPrivacyData(bytes.NewReader(data))
	if err != nil {
		return nil, 0, nil, err
	}
	if !mayContainLocationData {
		return newOutboundJSONBody(data)
	}
	data, _, err = FilterUpstreamLocationData(data, channelUsesProxy...)
	if err != nil {
		return nil, 0, nil, err
	}
	return newOutboundJSONBody(data)
}

func newOutboundJSONBody(data []byte) (body io.Reader, size int64, closer io.Closer, err error) {
	storage, err := common.CreateBodyStorage(data)
	if err != nil {
		return nil, 0, nil, err
	}
	return common.ReaderOnly(storage), storage.Size(), storage, nil
}

// NewPrivacyFilteredPassThroughJSONBody applies the same outbound privacy
// policy to pass-through requests as converted requests.
func NewPrivacyFilteredPassThroughJSONBody(storage common.BodyStorage, channelUsesProxy ...bool) (body io.Reader, size int64, closer io.Closer, err error) {
	mayContainLocationData, err := storageMayContainLocationPrivacyData(storage)
	if err != nil {
		return nil, 0, nil, err
	}
	if !mayContainLocationData {
		return common.ReaderOnly(storage), storage.Size(), nil, nil
	}
	data, err := storage.Bytes()
	if err != nil {
		return nil, 0, nil, err
	}
	filtered, changed, err := FilterUpstreamLocationData(data, channelUsesProxy...)
	if err != nil {
		return nil, 0, nil, err
	}
	if !changed {
		return common.ReaderOnly(storage), storage.Size(), nil, nil
	}
	return newOutboundJSONBody(filtered)
}

const maxPrivacyCandidateKeyBytes = 1024

func storageMayContainLocationPrivacyData(storage common.BodyStorage) (mayContain bool, err error) {
	return readerMayContainLocationPrivacyData(storage)
}

func readerMayContainLocationPrivacyData(storage io.ReadSeeker) (mayContain bool, err error) {
	originalOffset, err := storage.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, err
	}
	if _, err = storage.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	defer func() {
		_, restoreErr := storage.Seek(originalOffset, io.SeekStart)
		if err == nil && restoreErr != nil {
			err = restoreErr
		}
	}()

	reader := bufio.NewReaderSize(storage, 32*1024)
	rawKey := make([]byte, 0, 64)
	for {
		current, readErr := reader.ReadByte()
		if readErr != nil {
			if readErr == io.EOF {
				return false, nil
			}
			return false, readErr
		}
		if current != '"' {
			continue
		}

		rawKey = append(rawKey[:0], '"')
		overflow := false
		escaped := false
		for {
			current, readErr = reader.ReadByte()
			if readErr != nil {
				if readErr == io.EOF {
					return false, nil
				}
				return false, readErr
			}
			if !overflow {
				if len(rawKey) < maxPrivacyCandidateKeyBytes {
					rawKey = append(rawKey, current)
				} else {
					overflow = true
				}
			}
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
				continue
			}
			if current == '"' {
				break
			}
		}

		for {
			current, readErr = reader.ReadByte()
			if readErr != nil {
				if readErr == io.EOF {
					return false, nil
				}
				return false, readErr
			}
			if current != ' ' && current != '\t' && current != '\r' && current != '\n' {
				break
			}
		}
		if current != ':' || overflow {
			continue
		}

		var key string
		if err := common.Unmarshal(rawKey, &key); err != nil {
			continue
		}
		if isLocationPrivacyCandidateKey(key) {
			return true, nil
		}
	}
}
