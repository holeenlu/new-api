package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/oauthcred"
)

// SubscriptionOAuthCredentialFingerprint returns the stable, non-secret
// identity used by subscription OAuth capacity, retry, and quarantine policy.
// Channel and key indexes are only fallbacks for an empty credential.
func SubscriptionOAuthCredentialFingerprint(channelType, channelID, keyIndex int, key string) string {
	identity := strings.TrimSpace(key)
	switch channelType {
	case constant.ChannelTypeCodex:
		var credential struct {
			AccountID string `json:"account_id"`
		}
		if common.Unmarshal([]byte(identity), &credential) == nil && strings.TrimSpace(credential.AccountID) != "" {
			identity = "account:" + strings.TrimSpace(credential.AccountID)
		} else if identity != "" {
			identity = "credential:" + identity
		}
	case constant.ChannelTypeClaudeCode:
		identity = oauthcred.NormalizeClaudeCodeToken(identity)
		if identity != "" {
			identity = "credential:" + identity
		}
	}
	if identity == "" {
		identity = fmt.Sprintf("channel:%d:key:%d", channelID, keyIndex)
	}
	digest := sha256.Sum256([]byte(strconv.Itoa(channelType) + ":" + identity))
	return hex.EncodeToString(digest[:])
}

type QuarantinedChannelCredential struct {
	ChannelID   int
	ChannelName string
	KeyIndex    int
	MultiKey    bool
}

// QuarantineSubscriptionOAuthCredential disables every database reference to
// one rejected subscription credential. The credential identity is global to
// the process, while routing groups remain request-local failover boundaries.
func QuarantineSubscriptionOAuthCredential(channelType int, fingerprint, reason string) ([]QuarantinedChannelCredential, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return nil, fmt.Errorf("subscription OAuth credential fingerprint is empty")
	}

	tx := DB.Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	defer func() {
		if recoverValue := recover(); recoverValue != nil {
			tx.Rollback()
			panic(recoverValue)
		}
	}()

	var channels []*Channel
	if err := lockForUpdate(tx).
		Where("type = ?", channelType).
		Find(&channels).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	affected := make([]QuarantinedChannelCredential, 0)
	affectedChannelIDs := make(map[int]struct{})
	for _, channel := range channels {
		keys := channel.GetKeys()
		if !channel.ChannelInfo.IsMultiKey {
			keys = []string{channel.Key}
		}

		channelChanged := false
		for keyIndex, key := range keys {
			if SubscriptionOAuthCredentialFingerprint(channel.Type, channel.Id, keyIndex, key) != fingerprint {
				continue
			}
			if channel.ChannelInfo.IsMultiKey {
				if current, exists := channel.ChannelInfo.MultiKeyStatusList[keyIndex]; exists && current == common.ChannelStatusManuallyDisabled {
					continue
				}
				setMultiKeyStatus(channel, keyIndex, common.ChannelStatusManuallyDisabled, reason)
			} else {
				if channel.Status == common.ChannelStatusManuallyDisabled {
					continue
				}
				channel.Status = common.ChannelStatusManuallyDisabled
				info := channel.GetOtherInfo()
				info["status_reason"] = reason
				info["status_time"] = common.GetTimestamp()
				channel.SetOtherInfo(info)
			}
			channelChanged = true
			affected = append(affected, QuarantinedChannelCredential{
				ChannelID:   channel.Id,
				ChannelName: channel.Name,
				KeyIndex:    keyIndex,
				MultiKey:    channel.ChannelInfo.IsMultiKey,
			})
		}
		if !channelChanged {
			continue
		}
		if err := tx.Model(channel).
			Select("status", "channel_info", "other_info").
			Updates(channel).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
		affectedChannelIDs[channel.Id] = struct{}{}
	}

	for channelID := range affectedChannelIDs {
		var channelStatus int
		for _, channel := range channels {
			if channel.Id == channelID {
				channelStatus = channel.Status
				break
			}
		}
		if channelStatus != common.ChannelStatusEnabled {
			if err := tx.Model(&Ability{}).
				Where("channel_id = ?", channelID).
				Select("enabled").
				Update("enabled", false).Error; err != nil {
				tx.Rollback()
				return nil, err
			}
		}
	}

	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	if len(affected) > 0 && common.MemoryCacheEnabled {
		InitChannelCache()
	}
	return affected, nil
}
