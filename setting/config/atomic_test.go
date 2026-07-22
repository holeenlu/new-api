package config

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type atomicTestSetting struct {
	Enabled  bool   `json:"enabled"`
	Revision int    `json:"revision"`
	Bucket   string `json:"bucket"`
}

func TestAtomicConfigRejectsAliasedSnapshotTypes(t *testing.T) {
	tests := []struct {
		name string
		new  func()
	}{
		{
			name: "pointer generic type",
			new: func() {
				_ = NewAtomicConfig(&atomicTestSetting{})
			},
		},
		{
			name: "pointer field",
			new: func() {
				_ = NewAtomicConfig(struct{ Revision *int }{})
			},
		},
		{
			name: "slice field",
			new: func() {
				_ = NewAtomicConfig(struct{ Revisions []int }{})
			},
		},
		{
			name: "scalar generic type",
			new: func() {
				_ = NewAtomicConfig(1)
			},
		},
		{
			name: "array field unsupported by config reflection",
			new: func() {
				_ = NewAtomicConfig(struct{ Revisions [2]int }{})
			},
		},
		{
			name: "struct with unexported state",
			new: func() {
				_ = NewAtomicConfig(struct{ Lock sync.Mutex }{})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Panics(t, test.new)
		})
	}
}

func TestZeroValueAtomicConfigCannotBypassSnapshotTypeValidation(t *testing.T) {
	type unsafeSetting struct {
		Values map[string]int `json:"values"`
	}
	var setting AtomicConfig[unsafeSetting]

	require.Error(t, setting.updateFromConfigMap(map[string]string{"values": `{"one":1}`}))
	_, err := ConfigToMap(&setting)
	require.Error(t, err)
	assert.Equal(t, unsafeSetting{}, setting.Load())
}

func TestNilAtomicConfigHasDefinedReadAndUpdateBehavior(t *testing.T) {
	var setting *AtomicConfig[atomicTestSetting]

	assert.Equal(t, atomicTestSetting{}, setting.Load())
	require.Error(t, setting.updateFromConfigMap(map[string]string{"revision": "2"}))
}

func TestAtomicConfigSupportsManagerLoadAndExport(t *testing.T) {
	setting := NewAtomicConfig(atomicTestSetting{
		Enabled:  true,
		Revision: 1,
		Bucket:   "hour",
	})
	manager := NewConfigManager()
	manager.Register("metrics", setting)

	require.NoError(t, manager.LoadFromDB(map[string]string{
		"metrics.enabled":  "false",
		"metrics.revision": "2",
		"metrics.bucket":   "minute",
	}))

	assert.Equal(t, atomicTestSetting{
		Enabled:  false,
		Revision: 2,
		Bucket:   "minute",
	}, setting.Load())
	assert.Equal(t, map[string]string{
		"metrics.enabled":  "false",
		"metrics.revision": "2",
		"metrics.bucket":   "minute",
	}, manager.ExportAllConfigs())
}

func TestAtomicConfigRejectsPartialInvalidUpdate(t *testing.T) {
	initial := atomicTestSetting{Enabled: true, Revision: 1, Bucket: "hour"}
	setting := NewAtomicConfig(initial)

	err := UpdateConfigFromMap(setting, map[string]string{
		"enabled":  "false",
		"revision": "not-an-integer",
	})

	require.Error(t, err)
	assert.Equal(t, initial, setting.Load())
}

func TestManagerLoadReturnsAtomicUpdateErrorWithoutPublishing(t *testing.T) {
	initial := atomicTestSetting{Enabled: true, Revision: 1, Bucket: "hour"}
	setting := NewAtomicConfig(initial)
	manager := NewConfigManager()
	manager.Register("metrics", setting)

	err := manager.LoadFromDB(map[string]string{
		"metrics.enabled":  "false",
		"metrics.revision": "not-an-integer",
	})

	require.Error(t, err)
	assert.Equal(t, initial, setting.Load())
}

func TestExportAllConfigsCheckedReturnsManagedSnapshotError(t *testing.T) {
	type unsafeSetting struct {
		Values map[string]int `json:"values"`
	}
	var invalid AtomicConfig[unsafeSetting]
	manager := NewConfigManager()
	manager.Register("invalid", &invalid)

	exported, err := manager.ExportAllConfigsChecked()

	require.Error(t, err)
	assert.Empty(t, exported)
}

func TestAtomicConfigPublishesConsistentSnapshotsDuringHotUpdates(t *testing.T) {
	first := atomicTestSetting{Enabled: true, Revision: 1, Bucket: "hour"}
	second := atomicTestSetting{Enabled: false, Revision: 2, Bucket: "minute"}
	setting := NewAtomicConfig(first)
	manager := NewConfigManager()
	manager.Register("metrics", setting)

	updates := []map[string]string{
		{
			"metrics.enabled":  "true",
			"metrics.revision": "1",
			"metrics.bucket":   "hour",
		},
		{
			"metrics.enabled":  "false",
			"metrics.revision": "2",
			"metrics.bucket":   "minute",
		},
	}

	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	observed := make(chan struct{}, 2)
	done := make(chan struct{})
	readerResults := make(chan error, 2)
	var readers sync.WaitGroup
	for range 2 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			ready <- struct{}{}
			firstSnapshot := setting.Load()
			observed <- struct{}{}
			if firstSnapshot != first && firstSnapshot != second {
				readerResults <- fmt.Errorf("observed partial configuration snapshot: %+v", firstSnapshot)
				return
			}
			for {
				select {
				case <-done:
					readerResults <- nil
					return
				default:
				}
				snapshot := setting.Load()
				if snapshot != first && snapshot != second {
					readerResults <- fmt.Errorf("observed partial configuration snapshot: %+v", snapshot)
					return
				}
			}
		}()
	}

	close(start)
	for range 2 {
		<-ready
	}
	for range 2 {
		<-observed
	}
	var updateErr error
	for i := 0; i < 64; i++ {
		if err := manager.LoadFromDB(updates[i%len(updates)]); err != nil {
			updateErr = err
			break
		}
	}
	close(done)
	readers.Wait()
	close(readerResults)

	require.NoError(t, updateErr)
	for err := range readerResults {
		require.NoError(t, err)
	}
	assert.Equal(t, second, setting.Load())
}

func TestAtomicConfigConcurrentFieldUpdatesDoNotLoseChanges(t *testing.T) {
	setting := NewAtomicConfig(atomicTestSetting{Enabled: true, Revision: 1, Bucket: "hour"})
	start := make(chan struct{})
	errs := make(chan error, 2)

	go func() {
		<-start
		errs <- setting.updateFromConfigMap(map[string]string{"revision": "2"})
	}()
	go func() {
		<-start
		errs <- setting.updateFromConfigMap(map[string]string{"bucket": "minute"})
	}()
	close(start)

	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	assert.Equal(t, atomicTestSetting{Enabled: true, Revision: 2, Bucket: "minute"}, setting.Load())
}
