package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/require"
)

func prepareOptionBatchTest(t *testing.T) {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}))
	common.OptionMapRWMutex.Lock()
	originalOptionMap := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = originalOptionMap
		common.OptionMapRWMutex.Unlock()
	})
}

func TestUpdateModelPricingOptionsPersistsValidatedBatch(t *testing.T) {
	prepareOptionBatchTest(t)
	modelRatio := ratio_setting.ModelRatio2JSONString()
	modelPrice := ratio_setting.ModelPrice2JSONString()
	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/option/model-pricing", modelPricingOptionsUpdateRequest{
		Options: map[string]string{
			"ModelRatio": modelRatio,
			"ModelPrice": modelPrice,
		},
	}, 1)

	UpdateModelPricingOptions(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
	var options []model.Option
	require.NoError(t, model.DB.Order("key asc").Find(&options).Error)
	require.Len(t, options, 2)
}

func TestUpdateModelPricingOptionsRejectsWholeInvalidBatch(t *testing.T) {
	prepareOptionBatchTest(t)
	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/option/model-pricing", modelPricingOptionsUpdateRequest{
		Options: map[string]string{
			"ModelRatio": ratio_setting.ModelRatio2JSONString(),
			"ModelPrice": `{"broken":"not-a-number"}`,
		},
	}, 1)

	UpdateModelPricingOptions(ctx)

	response := decodeAPIResponse(t, recorder)
	require.False(t, response.Success)
	var count int64
	require.NoError(t, model.DB.Model(&model.Option{}).Count(&count).Error)
	require.Zero(t, count)
}
