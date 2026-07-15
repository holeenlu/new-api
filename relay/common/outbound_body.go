package common

import (
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

// NewPrivacyFilteredPassThroughJSONBody avoids copying pass-through bodies when
// no protected location data is present, while still enforcing the same final
// outbound policy as converted requests.
func NewPrivacyFilteredPassThroughJSONBody(storage common.BodyStorage, channelUsesProxy ...bool) (body io.Reader, size int64, closer io.Closer, err error) {
	mayContainSensitiveData, err := storageMayContainSensitiveLocationData(storage)
	if err != nil {
		return nil, 0, nil, err
	}
	if !mayContainSensitiveData {
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

func storageMayContainSensitiveLocationData(storage common.BodyStorage) (bool, error) {
	if !storage.IsDisk() {
		data, err := storage.Bytes()
		if err != nil {
			return false, err
		}
		return mayContainSensitiveLocationData(data), nil
	}

	originalOffset, err := storage.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, err
	}
	if _, err := storage.Seek(0, io.SeekStart); err != nil {
		return false, err
	}

	maxMarkerLength := 0
	for _, marker := range sensitiveLocationDataMarkers {
		if len(marker) > maxMarkerLength {
			maxMarkerLength = len(marker)
		}
	}
	window := make([]byte, 32*1024+maxMarkerLength-1)
	overlapLength := 0
	found := false
	for {
		count, readErr := storage.Read(window[overlapLength:])
		if count > 0 {
			windowLength := overlapLength + count
			if mayContainSensitiveLocationData(window[:windowLength]) {
				found = true
				break
			}
			keep := maxMarkerLength - 1
			if keep > windowLength {
				keep = windowLength
			}
			copy(window[:keep], window[windowLength-keep:windowLength])
			overlapLength = keep
		}
		if readErr != nil {
			if readErr != io.EOF {
				_, _ = storage.Seek(originalOffset, io.SeekStart)
				return false, readErr
			}
			break
		}
	}
	if _, err := storage.Seek(originalOffset, io.SeekStart); err != nil {
		return false, err
	}
	return found, nil
}
