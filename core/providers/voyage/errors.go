package voyage

import (
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func parseVoyageError(resp *fasthttp.Response) *schemas.BifrostError {
	var body VoyageError
	err := providerUtils.HandleProviderAPIError(resp, &body)
	if body.Detail != "" {
		err.Error.Message = body.Detail
	}
	err.ExtraFields.ErrorType = schemas.ClassifyErrorType(err, schemas.EmbeddingRequest)
	return err
}
