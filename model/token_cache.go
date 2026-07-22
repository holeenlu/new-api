package model

import (
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
)

func ensureTokenCacheAvailable() error {
	if !common.RedisEnabled {
		return errors.New("redis is not enabled")
	}
	if common.RDB == nil {
		return errors.New("redis is enabled but not initialized")
	}
	return nil
}

func cacheSetToken(token Token) error {
	if err := ensureTokenCacheAvailable(); err != nil {
		return err
	}
	key := common.GenerateHMAC(token.Key)
	token.Clean()
	err := common.RedisHSetObj(fmt.Sprintf("token:%s", key), &token, time.Duration(common.RedisKeyCacheSeconds())*time.Second)
	if err != nil {
		return err
	}
	return nil
}

func cacheDeleteToken(key string) error {
	if err := ensureTokenCacheAvailable(); err != nil {
		return err
	}
	key = common.GenerateHMAC(key)
	err := common.RedisDelKey(fmt.Sprintf("token:%s", key))
	if err != nil {
		return err
	}
	return nil
}

func cacheSetTokenField(key string, field string, value string) error {
	if err := ensureTokenCacheAvailable(); err != nil {
		return err
	}
	key = common.GenerateHMAC(key)
	err := common.RedisHSetField(fmt.Sprintf("token:%s", key), field, value)
	if err != nil {
		return err
	}
	return nil
}

// CacheGetTokenByKey 从缓存中获取 token，如果缓存中不存在，则从数据库中获取
func cacheGetTokenByKey(key string) (*Token, error) {
	hmacKey := common.GenerateHMAC(key)
	if err := ensureTokenCacheAvailable(); err != nil {
		return nil, err
	}
	var token Token
	err := common.RedisHGetObj(fmt.Sprintf("token:%s", hmacKey), &token)
	if err != nil {
		return nil, err
	}
	if token.Id <= 0 {
		// Never authenticate an incomplete cache entry. Remove it so the caller can
		// rebuild the full token identity from the database.
		_ = cacheDeleteToken(key)
		return nil, errors.New("token cache entry is incomplete")
	}
	token.Key = key
	return &token, nil
}
