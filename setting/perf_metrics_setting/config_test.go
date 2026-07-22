package perf_metrics_setting

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPerfMetricsSettingHotUpdatePublishesSnapshot(t *testing.T) {
	original := GetSetting()
	registered := config.GlobalConfig.Get("perf_metrics_setting")
	require.NotNil(t, registered)
	t.Cleanup(func() {
		assert.NoError(t, config.UpdateConfigFromMap(registered, map[string]string{
			"enabled":        strconv.FormatBool(original.Enabled),
			"flush_interval": strconv.Itoa(original.FlushInterval),
			"bucket_time":    original.BucketTime,
			"retention_days": strconv.Itoa(original.RetentionDays),
		}))
	})

	require.NoError(t, config.UpdateConfigFromMap(registered, map[string]string{
		"enabled":        "false",
		"flush_interval": "0",
		"bucket_time":    "5min",
		"retention_days": "14",
	}))

	assert.Equal(t, PerfMetricsSetting{
		Enabled:       false,
		FlushInterval: 0,
		BucketTime:    "5min",
		RetentionDays: 14,
	}, GetSetting())
	assert.Equal(t, int64(300), GetBucketSeconds())
	assert.Equal(t, 1, GetFlushIntervalMinutes())
}

func TestPerfMetricsSettingRejectsDurationOverflowWithoutPublishing(t *testing.T) {
	original := GetSetting()
	registered := config.GlobalConfig.Get("perf_metrics_setting")
	require.NotNil(t, registered)

	err := config.UpdateConfigFromMap(registered, map[string]string{
		"flush_interval": strconv.FormatInt(math.MaxInt64, 10),
	})

	require.Error(t, err)
	assert.Equal(t, original, GetSetting())
}

func TestPerfMetricsDurationsSaturateUnvalidatedValues(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	setting := PerfMetricsSetting{
		FlushInterval: maxInt,
		RetentionDays: maxInt,
	}

	assert.Equal(t, time.Duration(maxFlushIntervalMinutes)*time.Minute, setting.FlushIntervalDuration())
	assert.Equal(t, time.Duration(maxRetentionDays)*24*time.Hour, setting.RetentionDuration())
}
