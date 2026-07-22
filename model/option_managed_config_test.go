package model

import (
	"runtime"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	appsetting "github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type managedOptionTestSetting struct {
	Enabled  bool `json:"enabled"`
	Revision int  `json:"revision"`
}

func TestUpdateOptionsBulkRejectsInvalidManagedConfigBeforePersisting(t *testing.T) {
	db := prepareRetryStatusOptionTest(t)
	original := config.GlobalConfig.Get("performance_setting")
	initial := managedOptionTestSetting{Enabled: true, Revision: 1}
	managed := config.NewAtomicConfig(initial)
	config.GlobalConfig.Register("performance_setting", managed)
	t.Cleanup(func() {
		config.GlobalConfig.Register("performance_setting", original)
	})

	err := UpdateOptionsBulk(map[string]string{
		"performance_setting.enabled":  "false",
		"performance_setting.revision": "not-an-integer",
	})

	require.Error(t, err)
	assert.Equal(t, initial, managed.Load())
	var count int64
	require.NoError(t, db.Model(&Option{}).Count(&count).Error)
	assert.Zero(t, count)

	require.Error(t, updateOptionMap("performance_setting.revision", "still-not-an-integer"))
	_, published := common.OptionMap["performance_setting.revision"]
	assert.False(t, published)

	require.NoError(t, db.Create([]Option{
		{Key: "performance_setting.enabled", Value: "false"},
		{Key: "performance_setting.revision", Value: "invalid-from-database"},
	}).Error)
	loadOptionsFromDatabase()
	assert.Equal(t, initial, managed.Load(), "database sync must reject the whole managed module")
	_, enabledPublished := common.OptionMap["performance_setting.enabled"]
	_, revisionPublished := common.OptionMap["performance_setting.revision"]
	assert.False(t, enabledPublished)
	assert.False(t, revisionPublished)
}

func TestLoadOptionsIsolatesInvalidRegisteredModule(t *testing.T) {
	db := prepareRetryStatusOptionTest(t)
	original := config.GlobalConfig.Get("performance_setting")
	initial := managedOptionTestSetting{Enabled: true, Revision: 1}
	managed := config.NewAtomicConfig(initial)
	config.GlobalConfig.Register("performance_setting", managed)
	t.Cleanup(func() {
		config.GlobalConfig.Register("performance_setting", original)
	})

	require.NoError(t, db.Create([]Option{
		{Key: "performance_setting.enabled", Value: "false"},
		{Key: "performance_setting.revision", Value: "invalid-from-database"},
		{Key: "Notice", Value: "valid-independent-option"},
	}).Error)

	loadOptionsFromDatabase()

	assert.Equal(t, initial, managed.Load(), "one invalid field must reject its complete module")
	_, enabledPublished := common.OptionMap["performance_setting.enabled"]
	_, revisionPublished := common.OptionMap["performance_setting.revision"]
	assert.False(t, enabledPublished)
	assert.False(t, revisionPublished)
	assert.Equal(t, "valid-independent-option", common.OptionMap["Notice"],
		"a corrupt module must not block unrelated database options")
}

func TestUpdateOptionsBulkRejectsInvalidLegacyConfigBeforePersisting(t *testing.T) {
	db := prepareRetryStatusOptionTest(t)
	original := config.GlobalConfig.Get("performance_setting")
	initial := managedOptionTestSetting{Enabled: true, Revision: 1}
	legacy := initial
	config.GlobalConfig.Register("performance_setting", &legacy)
	t.Cleanup(func() {
		config.GlobalConfig.Register("performance_setting", original)
	})

	err := UpdateOptionsBulk(map[string]string{
		"performance_setting.enabled":  "not-a-boolean",
		"performance_setting.revision": "2",
	})

	require.Error(t, err)
	assert.Equal(t, initial, legacy)
	var count int64
	require.NoError(t, db.Model(&Option{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestInvalidLegacyOptionCannotPartiallyPublishOrPersistBatch(t *testing.T) {
	db := prepareRetryStatusOptionTest(t)
	originalChats := appsetting.Chats2JsonString()
	originalSMTPPort := common.SMTPPort
	const initialChats = `[{"name":"before"}]`
	require.NoError(t, appsetting.UpdateChatsByJsonString(initialChats))
	common.OptionMapRWMutex.Lock()
	common.OptionMap["Chats"] = initialChats
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		require.NoError(t, appsetting.UpdateChatsByJsonString(originalChats))
		common.SMTPPort = originalSMTPPort
	})

	values := map[string]string{
		"Chats":    `{invalid-json`,
		"SMTPPort": "2525",
	}
	require.Error(t, UpdateOptionsBulk(values))
	assert.Equal(t, initialChats, appsetting.Chats2JsonString())
	assert.Equal(t, originalSMTPPort, common.SMTPPort)
	common.OptionMapRWMutex.RLock()
	storedChats := common.OptionMap["Chats"]
	_, smtpPublished := common.OptionMap["SMTPPort"]
	common.OptionMapRWMutex.RUnlock()
	assert.Equal(t, initialChats, storedChats)
	assert.False(t, smtpPublished)

	var count int64
	require.NoError(t, db.Model(&Option{}).Count(&count).Error)
	assert.Zero(t, count)

	require.Error(t, updateOptionMaps(values))
	assert.Equal(t, initialChats, appsetting.Chats2JsonString())
	assert.Equal(t, originalSMTPPort, common.SMTPPort)
}

func TestUpdateOptionsBulkRejectsMalformedLegacyScalarsBeforePersisting(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "integer", key: "SMTPPort", value: "not-an-integer"},
		{name: "boolean", key: "LogConsumeEnabled", value: "yes"},
		{name: "finite number", key: "Price", value: "NaN"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := prepareRetryStatusOptionTest(t)

			require.Error(t, UpdateOptionsBulk(map[string]string{test.key: test.value}))

			var count int64
			require.NoError(t, db.Model(&Option{}).Count(&count).Error)
			assert.Zero(t, count)
			_, published := common.OptionMap[test.key]
			assert.False(t, published)
		})
	}
}

func TestLoadOptionsPrefersRegisteredQuotaDisplayOverLegacyBridge(t *testing.T) {
	db := prepareRetryStatusOptionTest(t)
	original := operation_setting.GetQuotaDisplayType()
	t.Cleanup(func() {
		require.NoError(t, config.UpdateConfigFromMap(
			config.GlobalConfig.Get("general_setting"),
			map[string]string{"quota_display_type": original},
		))
	})
	require.NoError(t, db.Create([]Option{
		{Key: "DisplayInCurrencyEnabled", Value: "true"},
		{Key: "general_setting.quota_display_type", Value: operation_setting.QuotaDisplayTypeCNY},
	}).Error)

	loadOptionsFromDatabase()

	assert.Equal(t, operation_setting.QuotaDisplayTypeCNY, operation_setting.GetQuotaDisplayType())
	assert.Equal(t, operation_setting.QuotaDisplayTypeCNY, common.OptionMap["general_setting.quota_display_type"])
}

func TestLoadOptionsPublishesLegacyQuotaDisplayToCanonicalOption(t *testing.T) {
	db := prepareRetryStatusOptionTest(t)
	original := operation_setting.GetQuotaDisplayType()
	t.Cleanup(func() {
		require.NoError(t, config.UpdateConfigFromMap(
			config.GlobalConfig.Get("general_setting"),
			map[string]string{"quota_display_type": original},
		))
	})
	require.NoError(t, db.Create(&Option{
		Key:   "DisplayInCurrencyEnabled",
		Value: "false",
	}).Error)

	loadOptionsFromDatabase()

	assert.Equal(t, operation_setting.QuotaDisplayTypeTokens, operation_setting.GetQuotaDisplayType())
	assert.Equal(t, operation_setting.QuotaDisplayTypeTokens, common.OptionMap["general_setting.quota_display_type"])
}

func TestConcurrentOptionUpdateCannotBeOverwrittenByStaleDatabaseSnapshot(t *testing.T) {
	db := prepareRetryStatusOptionTest(t)
	previousMaxProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(previousMaxProcs)
	})
	original := config.GlobalConfig.Get("performance_setting")
	managed := config.NewAtomicConfig(managedOptionTestSetting{Enabled: true, Revision: 0})
	config.GlobalConfig.Register("performance_setting", managed)
	t.Cleanup(func() {
		config.GlobalConfig.Register("performance_setting", original)
	})

	const optionKey = "performance_setting.revision"
	require.NoError(t, db.Create(&Option{Key: optionKey, Value: "1"}).Error)

	snapshotRead := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseSnapshot)
		})
	}
	const callbackName = "test:pause_stale_option_snapshot"
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]*Option); !ok {
			return
		}
		close(snapshotRead)
		<-releaseSnapshot
	}))
	t.Cleanup(func() {
		release()
		_ = db.Callback().Query().Remove(callbackName)
	})

	loadDone := make(chan struct{})
	go func() {
		loadOptionsFromDatabase()
		close(loadDone)
	}()
	<-snapshotRead

	updateStarted := make(chan struct{})
	updateDone := make(chan error, 1)
	go func() {
		// With one scheduler P, this handoff lets the update run until it either
		// finishes or waits behind the in-flight snapshot before the test releases it.
		updateStarted <- struct{}{}
		updateDone <- UpdateOptionsBulk(map[string]string{optionKey: "2"})
	}()
	<-updateStarted
	release()

	<-loadDone
	require.NoError(t, <-updateDone)

	var stored Option
	require.NoError(t, db.First(&stored, "key = ?", optionKey).Error)
	assert.Equal(t, "2", stored.Value)
	assert.Equal(t, managedOptionTestSetting{Enabled: true, Revision: 2}, managed.Load())
	assert.Equal(t, "2", common.OptionMap[optionKey])
}
