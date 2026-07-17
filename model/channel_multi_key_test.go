package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/require"
)

func TestGetNextEnabledKeyExcludesKeysAlreadyUsedByRequest(t *testing.T) {
	channel := &Channel{
		Id:  1001,
		Key: "key-1\nkey-2\nkey-3",
		ChannelInfo: ChannelInfo{
			IsMultiKey:         true,
			MultiKeyMode:       constant.MultiKeyModeRandom,
			MultiKeyStatusList: map[int]int{1: 2},
		},
	}

	key, index, err := channel.GetNextEnabledKey(map[int]struct{}{0: {}})

	require.Nil(t, err)
	require.Equal(t, "key-3", key)
	require.Equal(t, 2, index)

	_, _, err = channel.GetNextEnabledKey(map[int]struct{}{0: {}, 2: {}})
	require.NotNil(t, err)
}

func TestGetEnabledKeyAtHonorsMultiKeyStatus(t *testing.T) {
	channel := &Channel{
		Id:  1002,
		Key: "key-1\nkey-2",
		ChannelInfo: ChannelInfo{
			IsMultiKey:         true,
			MultiKeyStatusList: map[int]int{1: common.ChannelStatusAutoDisabled},
		},
	}

	key, err := channel.GetEnabledKeyAt(0)
	require.Nil(t, err)
	require.Equal(t, "key-1", key)

	_, err = channel.GetEnabledKeyAt(1)
	require.NotNil(t, err)

	_, err = channel.GetEnabledKeyAt(2)
	require.NotNil(t, err)
}
