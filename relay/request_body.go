package relay

import (
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// buildPrivacyFilteredPassThroughBody assembles the upstream request body for a
// pass-through relay: it reads the stored client JSON body, applies the outbound
// location-privacy filter, records the resulting size on info, and returns a
// cleanup function the caller must defer. It is the single owner of that
// sequence for the JSON relay handlers (chat, Claude, Gemini, Responses) so the
// privacy guarantee stays uniform across them.
//
// Image and rerank handlers deliberately do not use this helper: image requests
// may be multipart/form-data, which a JSON filter would corrupt, so they forward
// the raw stored body.
func buildPrivacyFilteredPassThroughBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, func(), *types.NewAPIError) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, nil, types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	body, size, closer, err := relaycommon.NewPrivacyFilteredPassThroughJSONBody(storage, info.ChannelSetting.Proxy)
	if err != nil {
		return nil, nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	info.UpstreamRequestBodySize = size
	cleanup := func() {}
	if closer != nil {
		cleanup = func() { _ = closer.Close() }
	}
	return body, cleanup, nil
}
