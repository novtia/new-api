package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupNovelAISubscriptionTest(t *testing.T) {
	t.Helper()
	originalDB := model.DB
	originalLogDB := model.LOG_DB
	originalMemoryCache := common.MemoryCacheEnabled
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() {
		model.DB = originalDB
		model.LOG_DB = originalLogDB
		common.MemoryCacheEnabled = originalMemoryCache
		_ = setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups)
		_ = ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios)
		model.ResetPoolLinkCache()
		if originalMemoryCache && originalDB != nil &&
			originalDB.Migrator().HasTable(&model.Channel{}) && originalDB.Migrator().HasTable(&model.Ability{}) {
			model.InitChannelCache()
		}
	})

	db := setupModelListControllerTestDB(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.Token{}, &model.PublicKeyClaim{}))
	common.MemoryCacheEnabled = true
	model.ResetPoolLinkCache()

	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":2}`))
}

func newNovelAISubscriptionRouter() *gin.Engine {
	r := gin.New()
	r.GET("/user/subscription", middleware.TokenAuthWithoutPoolLock(), NovelAISubscription)
	return r
}

func TestNovelAISubscriptionPassthroughAndPoolConcurrency(t *testing.T) {
	setupNovelAISubscriptionTest(t)

	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/user/subscription", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tier":3,"active":true,"expiresAt":1800000000,"trainingStepsLeft":{"fixedTrainingStepsLeft":9000,"purchasedTrainingSteps":120},"usage":{"percent":64,"purchasedPercent":114,"isNegative":false}}`))
	}))
	t.Cleanup(upstream.Close)

	user := &model.User{
		Username:    "nai_user",
		Password:    "password",
		Role:        common.RoleAdminUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AffCode:     "nai-user-aff",
		Quota:       100000,
		AccessToken: common.GetPointer("user-at"),
	}
	require.NoError(t, model.DB.Create(user).Error)

	pool := &model.Token{
		UserId:       user.Id,
		Key:          "pool" + common.GetRandomString(16),
		Name:         "pool",
		Status:       common.TokenStatusEnabled,
		ExpiredTime:  -1,
		RemainQuota:  50000,
		IsPublicPool: true,
	}
	require.NoError(t, model.DB.Create(pool).Error)
	child := &model.Token{
		UserId:         user.Id,
		Key:            "child" + common.GetRandomString(16),
		Name:           "child",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
		PoolParentId:   pool.Id,
		DailyCapQuota:  10000,
	}
	require.NoError(t, model.DB.Create(child).Error)

	priority := int64(10)
	weight := uint(100)
	ch := &model.Channel{
		Type:     constant.ChannelTypeNovelAI,
		Key:      "pst-upstream-secret-1\npst-upstream-secret-2",
		Status:   common.ChannelStatusEnabled,
		Name:     "novelai-official",
		BaseURL:  common.GetPointer(upstream.URL),
		Models:   "nai-diffusion-5-full,nai-diffusion-5-curated",
		Group:    "default",
		Priority: &priority,
		Weight:   &weight,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         2,
			MultiKeyMode:         constant.MultiKeyModePolling,
			MultiKeyPollingIndex: 1,
		},
	}
	require.NoError(t, model.DB.Create(ch).Error)
	require.NoError(t, ch.AddAbilities(nil))
	model.InitChannelCache()

	// Hold the single in-flight public pool slot to prove /user/subscription does
	// not contend on the generation concurrency lock.
	release, ok := model.AcquirePublicPoolCall(child.Id)
	require.True(t, ok)
	defer release()

	r := newNovelAISubscriptionRouter()
	req := httptest.NewRequest(http.MethodGet, "/user/subscription", nil)
	req.Header.Set("Authorization", "Bearer sk-"+child.Key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Bearer pst-upstream-secret-2", gotAuth)
	assert.JSONEq(t, `{"tier":3,"active":true,"expiresAt":1800000000,"trainingStepsLeft":{"fixedTrainingStepsLeft":9000,"purchasedTrainingSteps":120},"usage":{"percent":64,"purchasedPercent":114,"isNegative":false}}`, rec.Body.String())

	// Polling index must remain untouched by the read-only subscription check.
	cachedInfo, err := model.CacheGetChannelInfo(ch.Id)
	require.NoError(t, err)
	assert.Equal(t, 1, cachedInfo.MultiKeyPollingIndex)
}

func TestNovelAISubscriptionFallbackPinAndModelLimits(t *testing.T) {
	setupNovelAISubscriptionTest(t)

	badUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"statusCode":401,"message":"invalid token"}`))
	}))
	t.Cleanup(badUpstream.Close)

	goodUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tier":3,"active":true,"trainingStepsLeft":{"fixedTrainingStepsLeft":10000,"purchasedTrainingSteps":0},"usage":{"percent":100}}`))
	}))
	t.Cleanup(goodUpstream.Close)

	admin := &model.User{
		Username: "nai_admin",
		Password: "password",
		Role:     common.RoleAdminUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		AffCode:  "nai-admin-aff",
		Quota:    100000,
	}
	require.NoError(t, model.DB.Create(admin).Error)

	adminToken := &model.Token{
		UserId:         admin.Id,
		Key:            "adm" + common.GetRandomString(16),
		Name:           "admin-key",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, model.DB.Create(adminToken).Error)

	restrictedToken := &model.Token{
		UserId:             admin.Id,
		Key:                "lim" + common.GetRandomString(16),
		Name:               "restricted-key",
		Status:             common.TokenStatusEnabled,
		ExpiredTime:        -1,
		UnlimitedQuota:     true,
		ModelLimitsEnabled: true,
		ModelLimits:        "gpt-4o",
	}
	require.NoError(t, model.DB.Create(restrictedToken).Error)

	highPrio := int64(20)
	lowPrio := int64(5)
	chBad := &model.Channel{
		Type:     constant.ChannelTypeNovelAI,
		Key:      "pst-bad",
		Status:   common.ChannelStatusEnabled,
		Name:     "novelai-bad",
		BaseURL:  common.GetPointer(badUpstream.URL),
		Models:   "nai-diffusion-5-full",
		Group:    "default",
		Priority: &highPrio,
	}
	chGood := &model.Channel{
		Type:     constant.ChannelTypeNovelAI,
		Key:      "pst-good",
		Status:   common.ChannelStatusEnabled,
		Name:     "novelai-good",
		BaseURL:  common.GetPointer(goodUpstream.URL),
		Models:   "nai-diffusion-5-full",
		Group:    "default",
		Priority: &lowPrio,
	}
	chOpenAI := &model.Channel{
		Type:     constant.ChannelTypeOpenAI,
		Key:      "sk-openai",
		Status:   common.ChannelStatusEnabled,
		Name:     "openai-other",
		Models:   "gpt-4o",
		Group:    "default",
		Priority: &highPrio,
	}
	require.NoError(t, model.DB.Create(chBad).Error)
	require.NoError(t, chBad.AddAbilities(nil))
	require.NoError(t, model.DB.Create(chGood).Error)
	require.NoError(t, chGood.AddAbilities(nil))
	require.NoError(t, model.DB.Create(chOpenAI).Error)
	require.NoError(t, chOpenAI.AddAbilities(nil))
	model.InitChannelCache()

	r := newNovelAISubscriptionRouter()

	t.Run("falls back to secondary NovelAI channel when primary returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/user/subscription", nil)
		req.Header.Set("Authorization", "Bearer sk-"+adminToken.Key)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"tier":3,"active":true,"trainingStepsLeft":{"fixedTrainingStepsLeft":10000,"purchasedTrainingSteps":0},"usage":{"percent":100}}`, rec.Body.String())
	})

	t.Run("admin pin hits exact channel and forwards upstream error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/user/subscription", nil)
		req.Header.Set("Authorization", fmt.Sprintf("Bearer sk-%s-%d", adminToken.Key, chBad.Id))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.JSONEq(t, `{"statusCode":401,"message":"invalid token"}`, rec.Body.String())
	})

	t.Run("admin pin to non-NovelAI channel is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/user/subscription", nil)
		req.Header.Set("Authorization", fmt.Sprintf("Bearer sk-%s-%d", adminToken.Key, chOpenAI.Id))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, strings.ToLower(rec.Body.String()), "novelai")
	})

	t.Run("token model limits forbid channels with no allowed NovelAI model", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/user/subscription", nil)
		req.Header.Set("Authorization", "Bearer sk-"+restrictedToken.Key)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		require.Equal(t, http.StatusForbidden, rec.Code)
	})
}
