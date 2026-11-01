package service

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// imageCountedRequest is implemented by relay requests that generate images,
// so the daily image limit counts the same quantity the request is billed for.
type imageCountedRequest interface {
	ImageCount(useProviderParameters bool) (int, error)
}

// ReservePoolUsage counts one request, and the images it asks for, against a
// public-pool visitor key's daily limits. It runs for every request regardless
// of price: a free model reserves no quota, but its count must still be capped.
// Tokens that are not visitor keys pass through untouched.
func ReservePoolUsage(info *relaycommon.RelayInfo) *types.NewAPIError {
	if info == nil || info.IsPlayground || info.TokenId <= 0 || info.PoolUsageRequests > 0 {
		return nil
	}
	images := 0
	if request, ok := info.Request.(imageCountedRequest); ok {
		// An invalid quantity is rejected by the image validator; reserve nothing for it.
		if count, err := request.ImageCount(false); err == nil {
			images = count
		}
	}
	reserved, err := model.ReservePoolChildUsage(info.TokenId, 1, images)
	if errors.Is(err, model.ErrPublicPoolDailyCountLimit) || errors.Is(err, model.ErrPublicPoolDailyImageLimit) {
		return types.NewErrorWithStatusCode(err, types.ErrorCodePublicPoolLimitReached, http.StatusTooManyRequests, types.ErrOptionWithSkipRetry())
	}
	if err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeUpdateDataError, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}
	if reserved {
		info.PoolUsageRequests = 1
		info.PoolUsageImages = images
	}
	return nil
}

// RefundPoolUsage returns what ReservePoolUsage counted. It is safe to call
// more than once; only a whole failed request gives its count back.
func RefundPoolUsage(info *relaycommon.RelayInfo) {
	if info == nil || info.PoolUsageRequests <= 0 {
		return
	}
	model.RefundPoolChildUsage(info.TokenId, info.PoolUsageRequests, info.PoolUsageImages)
	info.PoolUsageRequests = 0
	info.PoolUsageImages = 0
}
