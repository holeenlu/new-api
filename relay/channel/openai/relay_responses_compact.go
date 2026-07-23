package openai

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func OaiResponsesCompactionHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	responseBody, apiErr := readBoundedResponsesBody(resp.Body)
	if apiErr != nil {
		return nil, apiErr
	}

	var compactResp dto.OpenAIResponsesCompactionResponse
	if err := common.Unmarshal(responseBody, &compactResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := compactResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, responsesWrappedError(info, resp, responseBody, oaiError)
	}
	if len(compactResp.Output) == 0 && compactResp.Usage == nil {
		// A "success" body with neither output nor usage (e.g. `{}`) is not a
		// compaction result; forwarding it would fake success on zero usage.
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("upstream returned a compaction body without output or usage"),
			types.ErrorCodeBadResponseBody,
			http.StatusBadGateway,
		)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)

	usage := normalizeResponsesUsage(compactResp.Usage)

	return &usage, nil
}
