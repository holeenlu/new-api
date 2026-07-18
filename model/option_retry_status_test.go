package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func prepareRetryStatusOptionTest(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Option{}))

	previousDB := DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	previousRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	common.OptionMapRWMutex.Lock()
	previousOptionMap := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, previousLogType)

	t.Cleanup(func() {
		DB = previousDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		operation_setting.AutomaticRetryStatusCodeRanges = previousRanges
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptionMap
		common.OptionMapRWMutex.Unlock()
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestUpdateOptionPersistsNormalizedRetryStatusCodes(t *testing.T) {
	db := prepareRetryStatusOptionTest(t)

	require.NoError(t, UpdateOption("AutomaticRetryStatusCodes", "500-599"))

	var option Option
	require.NoError(t, db.Where(&Option{Key: "AutomaticRetryStatusCodes"}).First(&option).Error)
	require.Equal(t, "500-503,505-523,525-599", option.Value)
	require.Equal(t, option.Value, common.OptionMap["AutomaticRetryStatusCodes"])
}

func TestLoadOptionsNormalizesLegacyRetryStatusCodes(t *testing.T) {
	db := prepareRetryStatusOptionTest(t)
	require.NoError(t, db.Create(&Option{
		Key:   "AutomaticRetryStatusCodes",
		Value: "429,500-599",
	}).Error)

	loadOptionsFromDatabase()

	var option Option
	require.NoError(t, db.Where(&Option{Key: "AutomaticRetryStatusCodes"}).First(&option).Error)
	require.Equal(t, "429,500-503,505-523,525-599", option.Value)
	require.Equal(t, option.Value, common.OptionMap["AutomaticRetryStatusCodes"])
	require.False(t, operation_setting.ShouldRetryByStatusCode(504))
	require.False(t, operation_setting.ShouldRetryByStatusCode(524))
}
