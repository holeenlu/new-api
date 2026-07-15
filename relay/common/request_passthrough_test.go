package common

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/stretchr/testify/assert"
)

func TestIsRequestPassThroughEnabled(t *testing.T) {
	originalGlobalPassThrough := model_setting.GetGlobalSettings().PassThroughRequestEnabled
	t.Cleanup(func() {
		model_setting.GetGlobalSettings().PassThroughRequestEnabled = originalGlobalPassThrough
	})

	tests := []struct {
		name               string
		info               *RelayInfo
		globalPassThrough  bool
		channelPassThrough bool
		want               bool
	}{
		{name: "nil relay info", globalPassThrough: true, want: false},
		{name: "missing channel metadata", info: &RelayInfo{}, globalPassThrough: true, want: false},
		{name: "standard channel disabled", info: &RelayInfo{ChannelMeta: &ChannelMeta{ChannelType: constant.ChannelTypeOpenAI}}, want: false},
		{name: "standard channel global enabled", info: &RelayInfo{ChannelMeta: &ChannelMeta{ChannelType: constant.ChannelTypeOpenAI}}, globalPassThrough: true, want: true},
		{name: "standard channel setting enabled", info: &RelayInfo{ChannelMeta: &ChannelMeta{ChannelType: constant.ChannelTypeOpenAI}}, channelPassThrough: true, want: true},
		{name: "Codex ignores global setting", info: &RelayInfo{ChannelMeta: &ChannelMeta{ChannelType: constant.ChannelTypeCodex}}, globalPassThrough: true, want: false},
		{name: "Codex ignores channel setting", info: &RelayInfo{ChannelMeta: &ChannelMeta{ChannelType: constant.ChannelTypeCodex}}, channelPassThrough: true, want: false},
		{name: "Claude Code ignores global setting", info: &RelayInfo{ChannelMeta: &ChannelMeta{ChannelType: constant.ChannelTypeClaudeCode}}, globalPassThrough: true, want: false},
		{name: "Claude Code ignores channel setting", info: &RelayInfo{ChannelMeta: &ChannelMeta{ChannelType: constant.ChannelTypeClaudeCode}}, channelPassThrough: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model_setting.GetGlobalSettings().PassThroughRequestEnabled = tt.globalPassThrough
			if tt.info != nil && tt.info.ChannelMeta != nil {
				tt.info.ChannelSetting = dto.ChannelSettings{PassThroughBodyEnabled: tt.channelPassThrough}
			}
			assert.Equal(t, tt.want, IsRequestPassThroughEnabled(tt.info))
		})
	}
}
