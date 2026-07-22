package perf_metrics_setting

import (
	"fmt"
	"math"
	"time"

	"github.com/QuantumNous/new-api/setting/config"
)

const (
	maxFlushIntervalMinutes = int64(math.MaxInt64 / int64(time.Minute))
	maxRetentionDays        = int64(math.MaxInt64 / int64(24*time.Hour))
)

type PerfMetricsSetting struct {
	Enabled       bool   `json:"enabled"`
	FlushInterval int    `json:"flush_interval"`
	BucketTime    string `json:"bucket_time"`
	RetentionDays int    `json:"retention_days"`
}

var perfMetricsSetting = config.NewValidatedAtomicConfig(PerfMetricsSetting{
	Enabled:       true,
	FlushInterval: 5,
	BucketTime:    "hour",
	RetentionDays: 0,
}, PerfMetricsSetting.Validate)

func init() {
	config.GlobalConfig.Register("perf_metrics_setting", perfMetricsSetting)
}

func GetSetting() PerfMetricsSetting {
	return perfMetricsSetting.Load()
}

func (setting PerfMetricsSetting) BucketSeconds() int64 {
	switch setting.BucketTime {
	case "minute":
		return 60
	case "5min":
		return 300
	case "hour":
		return 3600
	default:
		return 3600
	}
}

func (setting PerfMetricsSetting) FlushIntervalMinutes() int {
	interval := setting.FlushInterval
	if interval < 1 {
		return 1
	}
	if int64(interval) > maxFlushIntervalMinutes {
		limit := maxFlushIntervalMinutes
		return int(limit)
	}
	return interval
}

func (setting PerfMetricsSetting) FlushIntervalDuration() time.Duration {
	return time.Duration(setting.FlushIntervalMinutes()) * time.Minute
}

func (setting PerfMetricsSetting) RetentionDuration() time.Duration {
	if setting.RetentionDays <= 0 {
		return 0
	}
	days := setting.RetentionDays
	if int64(days) > maxRetentionDays {
		limit := maxRetentionDays
		days = int(limit)
	}
	return time.Duration(days) * 24 * time.Hour
}

func (setting PerfMetricsSetting) Validate() error {
	if int64(setting.FlushInterval) > maxFlushIntervalMinutes {
		return fmt.Errorf("flush_interval must not exceed %d minutes", maxFlushIntervalMinutes)
	}
	if int64(setting.RetentionDays) > maxRetentionDays {
		return fmt.Errorf("retention_days must not exceed %d days", maxRetentionDays)
	}
	return nil
}

func GetBucketSeconds() int64 {
	return GetSetting().BucketSeconds()
}

func GetFlushIntervalMinutes() int {
	return GetSetting().FlushIntervalMinutes()
}
