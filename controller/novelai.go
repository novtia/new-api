package controller

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

const (
	novelAISubscriptionMaxBytes = 1 << 20
	novelAISubscriptionTimeout  = 20 * time.Second
)

type novelAISubscriptionAttempt struct {
	statusCode  int
	contentType string
	body        []byte
	errStatus   int
	errMessage  string
}

// NovelAISubscription relays NovelAI's GET /user/subscription to an eligible
// NovelAI channel and returns the upstream subscription/Anlas/Opus JSON verbatim.
func NovelAISubscription(c *gin.Context) {
	candidates, errStatus, errMsg := resolveNovelAISubscriptionChannels(c)
	if errStatus != 0 {
		writeNovelAIError(c, errStatus, errMsg)
		return
	}

	var lastAttempt *novelAISubscriptionAttempt
	for i, ch := range candidates {
		attempt := fetchNovelAISubscriptionFromChannel(c, ch)
		if attempt.errStatus == 0 {
			// For 401/5xx from one upstream account, fall back to remaining
			// candidates if any are configured.
			if (attempt.statusCode == http.StatusUnauthorized || attempt.statusCode >= http.StatusInternalServerError) && i < len(candidates)-1 {
				lastAttempt = attempt
				continue
			}
			writeNovelAISubscriptionResponse(c, attempt)
			return
		}
		lastAttempt = attempt
	}

	if lastAttempt != nil {
		if lastAttempt.errStatus == 0 {
			writeNovelAISubscriptionResponse(c, lastAttempt)
			return
		}
		writeNovelAIError(c, lastAttempt.errStatus, lastAttempt.errMessage)
		return
	}
	writeNovelAIError(c, http.StatusServiceUnavailable, "no available NovelAI channel")
}

func resolveNovelAISubscriptionChannels(c *gin.Context) ([]*model.Channel, int, string) {
	constraints := service.GetChannelConstraints(c)
	if pin, found, overridden := constraints.ResolvedPin(); found {
		for _, lost := range overridden {
			logger.LogWarn(c, common.LogText(
				"channel pin overridden: winning_source=%s winning_channel_id=%d overridden_source=%s overridden_channel_id=%d",
				pin.Source, pin.ChannelId, lost.Source, lost.ChannelId,
			))
		}
		ch, err := model.CacheGetChannel(pin.ChannelId)
		if err != nil {
			return nil, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorInvalidChannelId)
		}
		if ch.Status != common.ChannelStatusEnabled {
			return nil, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorChannelDisabled)
		}
		if ch.Type != constant.ChannelTypeNovelAI {
			return nil, http.StatusBadRequest, "pinned channel is not a NovelAI channel"
		}
		if ok, _ := model.ChannelSatisfiesFilters(ch, "", constraints.Filters); !ok {
			usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
			return nil, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorNoAvailableChannel, map[string]any{"Group": usingGroup, "Model": "novelai"})
		}
		return []*model.Channel{ch}, 0, ""
	}

	modelLimitEnabled := common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled)
	var tokenModelLimit map[string]bool
	if modelLimitEnabled {
		raw, ok := common.GetContextKey(c, constant.ContextKeyTokenModelLimit)
		if !ok {
			return nil, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorTokenNoModelAccess)
		}
		tokenModelLimit, ok = raw.(map[string]bool)
		if !ok || len(tokenModelLimit) == 0 {
			return nil, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorTokenNoModelAccess)
		}
	}

	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	if usingGroup == "" {
		usingGroup = userGroup
	}
	var groups []string
	if usingGroup == "auto" {
		groups = service.GetRequestAutoGroups(c, userGroup)
		if len(groups) == 0 {
			return nil, http.StatusServiceUnavailable, "auto groups is not enabled"
		}
	} else {
		groups = []string{usingGroup}
	}

	candidates := make([]*model.Channel, 0)
	seenIDs := make(map[int]struct{})
	sawNovelAIChannel := false
	for _, group := range groups {
		groupChannels, err := model.GetEnabledChannelsByGroupAndType(group, constant.ChannelTypeNovelAI)
		if err != nil {
			logger.LogError(c, common.LogText("failed to query NovelAI channels for group %s: %v", group, err))
			return nil, http.StatusServiceUnavailable, i18n.T(c, i18n.MsgDistributorGetChannelFailed, map[string]any{"Group": group, "Model": "novelai", "Error": err.Error()})
		}
		filtered := make([]*model.Channel, 0, len(groupChannels))
		for _, ch := range groupChannels {
			if _, seen := seenIDs[ch.Id]; seen {
				continue
			}
			if ok, _ := model.ChannelSatisfiesFilters(ch, "", constraints.Filters); !ok {
				continue
			}
			sawNovelAIChannel = true
			if modelLimitEnabled && !channelAllowsAnyTokenModel(ch, tokenModelLimit) {
				continue
			}
			seenIDs[ch.Id] = struct{}{}
			filtered = append(filtered, ch)
		}
		candidates = append(candidates, orderNovelAIChannelsByPriorityAndWeight(filtered)...)
	}

	if len(candidates) == 0 {
		if modelLimitEnabled && sawNovelAIChannel {
			return nil, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorTokenNoModelAccess)
		}
		return nil, http.StatusServiceUnavailable, i18n.T(c, i18n.MsgDistributorNoAvailableChannel, map[string]any{"Group": usingGroup, "Model": "novelai"})
	}
	return candidates, 0, ""
}

func channelAllowsAnyTokenModel(ch *model.Channel, tokenModelLimit map[string]bool) bool {
	if ch == nil {
		return false
	}
	for _, m := range ch.GetModels() {
		m = strings.TrimSpace(m)
		if m != "" && middleware.TokenModelLimitAllows(tokenModelLimit, m) {
			return true
		}
	}
	return false
}

// orderNovelAIChannelsByPriorityAndWeight preserves priority tiers while
// shuffling channels within each tier proportionally to their weight.
func orderNovelAIChannelsByPriorityAndWeight(channels []*model.Channel) []*model.Channel {
	if len(channels) <= 1 {
		return slices.Clone(channels)
	}
	ordered := make([]*model.Channel, 0, len(channels))
	for start := 0; start < len(channels); {
		priority := channels[start].GetPriority()
		end := start + 1
		for end < len(channels) && channels[end].GetPriority() == priority {
			end++
		}
		tier := slices.Clone(channels[start:end])
		for len(tier) > 0 {
			totalWeight := 0
			for _, ch := range tier {
				totalWeight += ch.GetWeight() + 10
			}
			pick := common.GetRandomInt(totalWeight)
			chosen := 0
			for idx, ch := range tier {
				pick -= ch.GetWeight() + 10
				if pick < 0 {
					chosen = idx
					break
				}
			}
			ordered = append(ordered, tier[chosen])
			tier = slices.Delete(tier, chosen, chosen+1)
		}
		start = end
	}
	return ordered
}

func fetchNovelAISubscriptionFromChannel(c *gin.Context, ch *model.Channel) *novelAISubscriptionAttempt {
	apiKey, _, keyErr := ch.PeekEnabledKey()
	if keyErr != nil {
		return &novelAISubscriptionAttempt{errStatus: http.StatusServiceUnavailable, errMessage: keyErr.Error()}
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return &novelAISubscriptionAttempt{errStatus: http.StatusServiceUnavailable, errMessage: "novelai channel api key is empty"}
	}

	baseURL := strings.TrimSpace(ch.GetBaseURL())
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeNovelAI)
	}
	targetURL := strings.TrimRight(baseURL, "/") + "/user/subscription"

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, targetURL, nil)
	if err != nil {
		return &novelAISubscriptionAttempt{errStatus: http.StatusBadGateway, errMessage: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	channelSetting := ch.GetSetting()
	relayInfo := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:       ch.Id,
			ChannelType:     ch.Type,
			ChannelBaseUrl:  baseURL,
			ApiKey:          apiKey,
			ChannelSetting:  channelSetting,
			HeadersOverride: ch.GetHeaderOverride(),
		},
	}
	if err = relaychannel.ApplyHeaderOverride(relayInfo, c, req); err != nil {
		return &novelAISubscriptionAttempt{errStatus: http.StatusBadRequest, errMessage: err.Error()}
	}

	baseClient, err := service.GetHttpClientWithProxySettings(channelSetting.Proxy, channelSetting)
	if err != nil {
		return &novelAISubscriptionAttempt{errStatus: http.StatusBadGateway, errMessage: err.Error()}
	}
	client := *baseClient
	client.Timeout = novelAISubscriptionTimeout
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := client.Do(req)
	if err != nil {
		return &novelAISubscriptionAttempt{errStatus: http.StatusBadGateway, errMessage: err.Error()}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, novelAISubscriptionMaxBytes+1))
	if err != nil {
		return &novelAISubscriptionAttempt{errStatus: http.StatusBadGateway, errMessage: err.Error()}
	}
	if len(body) > novelAISubscriptionMaxBytes {
		return &novelAISubscriptionAttempt{errStatus: http.StatusBadGateway, errMessage: "novelai subscription response exceeds size limit"}
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	return &novelAISubscriptionAttempt{
		statusCode:  resp.StatusCode,
		contentType: contentType,
		body:        body,
	}
}

func writeNovelAISubscriptionResponse(c *gin.Context, attempt *novelAISubscriptionAttempt) {
	c.Header("Cache-Control", "no-store")
	c.Data(attempt.statusCode, attempt.contentType, attempt.body)
}

func writeNovelAIError(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, gin.H{
		"statusCode": statusCode,
		"message":    message,
	})
}
