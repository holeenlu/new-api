package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
)

func RefreshChannelProxyLocationProfiles(ctx context.Context) (int, error) {
	var channels []model.Channel
	if err := model.DB.Select("id", "setting").Find(&channels).Error; err != nil {
		return 0, err
	}

	proxies := make(map[string]struct{})
	for i := range channels {
		if channels[i].Setting == nil || strings.TrimSpace(*channels[i].Setting) == "" {
			continue
		}
		var setting dto.ChannelSettings
		if err := common.Unmarshal([]byte(*channels[i].Setting), &setting); err != nil {
			continue
		}
		if proxyURL := strings.TrimSpace(setting.Proxy); proxyURL != "" {
			proxies[proxyURL] = struct{}{}
		}
	}

	updated := 0
	var discoveryErrors []error
	for proxyURL := range proxies {
		client, err := GetHttpClientWithProxy(proxyURL)
		if err != nil {
			discoveryErrors = append(discoveryErrors, fmt.Errorf("create channel proxy client: %w", err))
			continue
		}
		profile, err := common.DiscoverChannelProxyLocationProfile(ctx, client)
		if err != nil {
			discoveryErrors = append(discoveryErrors, fmt.Errorf("discover channel proxy egress: %w", err))
			continue
		}
		if common.SetChannelProxyLocationProfile(proxyURL, profile) {
			updated++
		}
	}
	return updated, errors.Join(discoveryErrors...)
}
