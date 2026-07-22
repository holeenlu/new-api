package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupRelayTokenAuthCacheTest(t *testing.T) {
	t.Helper()
	previousDB := model.DB
	previousDatabaseType := common.MainDatabaseType()
	previousRedisEnabled := common.RedisEnabled
	previousRDB := common.RDB

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	common.RedisEnabled = true
	common.RDB = client
	t.Cleanup(func() {
		_ = client.Close()
		model.DB = previousDB
		common.SetMainDatabaseType(previousDatabaseType)
		common.RedisEnabled = previousRedisEnabled
		common.RDB = previousRDB
	})
}

func cacheTokenSnapshot(t *testing.T, token model.Token) {
	t.Helper()
	require.NoError(t, common.RedisHSetObj(
		"token:"+common.GenerateHMAC(token.Key),
		&token,
		time.Minute,
	))
}

func TestTokenAuthRejectsStalePositiveCacheForExhaustedToken(t *testing.T) {
	setupRelayTokenAuthCacheTest(t)

	const tokenKey = "staleexhaustedtoken"
	stored := model.Token{
		Id:          700,
		UserId:      702,
		Key:         tokenKey,
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 0,
	}
	require.NoError(t, model.DB.Create(&stored).Error)
	stale := stored
	stale.RemainQuota = 100
	cacheTokenSnapshot(t, stale)

	handlerCalled := false
	router := gin.New()
	router.GET("/free-model", TokenAuth(), func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/free-model", nil)
	request.Header.Set("Authorization", "Bearer sk-"+tokenKey)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.False(t, handlerCalled)
}

func TestTokenAuthReadOnlyRejectsStaleEnabledCacheForDisabledToken(t *testing.T) {
	setupRelayTokenAuthCacheTest(t)

	const tokenKey = "stalereadonlytoken"
	stored := model.Token{
		Id:          701,
		UserId:      702,
		Key:         tokenKey,
		Status:      common.TokenStatusDisabled,
		ExpiredTime: -1,
		RemainQuota: 100,
	}
	require.NoError(t, model.DB.Create(&stored).Error)
	stale := stored
	stale.Status = common.TokenStatusEnabled
	cacheTokenSnapshot(t, stale)

	handlerCalled := false
	router := gin.New()
	router.GET("/readonly", TokenAuthReadOnly(), func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/readonly", nil)
	request.Header.Set("Authorization", "Bearer sk-"+tokenKey)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.False(t, handlerCalled)
}
