package common

import (
	"testing"
	"time"

	"github.com/shirou/gopsutil/mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectSystemStatusWhenPerformanceMonitorDisabled(t *testing.T) {
	originalConfig := GetPerformanceMonitorConfig()
	originalCPUCollector := collectCPUPercent
	originalMemoryCollector := collectVirtualMemory
	originalDiskCollector := collectDiskSpaceInfo
	t.Cleanup(func() {
		SetPerformanceMonitorConfig(originalConfig)
		collectCPUPercent = originalCPUCollector
		collectVirtualMemory = originalMemoryCollector
		collectDiskSpaceInfo = originalDiskCollector
	})
	SetPerformanceMonitorConfig(PerformanceMonitorConfig{Enabled: false})
	collectCPUPercent = func(time.Duration, bool) ([]float64, error) {
		return []float64{12.5}, nil
	}
	collectVirtualMemory = func() (*mem.VirtualMemoryStat, error) {
		return &mem.VirtualMemoryStat{UsedPercent: 34.5}, nil
	}
	collectDiskSpaceInfo = func() DiskSpaceInfo {
		return DiskSpaceInfo{Total: 100, UsedPercent: 56.5}
	}

	status := CollectSystemStatus()

	require.Equal(t, 12.5, status.CPUUsage)
	assert.Equal(t, 34.5, status.MemoryUsage)
	assert.Equal(t, 56.5, status.DiskUsage)
}
