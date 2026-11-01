package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockDiscordUser struct {
	id       string
	username string
	member   bool
	joinedAt time.Time
}

type mockDiscord struct {
	mu    sync.Mutex
	users map[string]*mockDiscordUser // authorization code -> user
}

var snowflakeSeq int64

// makeSnowflake 按 Discord 雪花规则生成 ID；低 22 位用递增序号，保证同一毫秒创建的用户 ID 也不同。
func makeSnowflake(created time.Time) string {
	seq := atomic.AddInt64(&snowflakeSeq, 1) & 0x3FFFFF
	return strconv.FormatInt((created.UnixMilli()-1420070400000)<<22|seq, 10)
}

// newMockDiscord 启动一个本地 Discord API 替身，授权码即用户标识。
func newMockDiscord(t *testing.T) *mockDiscord {
	t.Helper()
	md := &mockDiscord{users: map[string]*mockDiscordUser{}}
	byToken := func(r *http.Request) *mockDiscordUser {
		md.mu.Lock()
		defer md.mu.Unlock()
		return md.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.PostForm.Get("code")
		md.mu.Lock()
		_, ok := md.users[code]
		md.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-" + code})
	})
	mux.HandleFunc("/users/@me", func(w http.ResponseWriter, r *http.Request) {
		u := byToken(r)
		if u == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": u.id, "username": u.username, "global_name": u.username})
	})
	mux.HandleFunc("/users/@me/guilds/", func(w http.ResponseWriter, r *http.Request) {
		u := byToken(r)
		if u == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/"+defaultPoolGuildID+"/member") || !u.member {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"joined_at": u.joinedAt.UTC().Format("2006-01-02T15:04:05.000000+00:00")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldBase := discordAPIBase
	discordAPIBase = srv.URL
	t.Cleanup(func() { discordAPIBase = oldBase })
	return md
}

func (md *mockDiscord) addUser(code, name string, accountAge, joinAge time.Duration, member bool) {
	md.mu.Lock()
	defer md.mu.Unlock()
	md.users[code] = &mockDiscordUser{
		id:       makeSnowflake(time.Now().Add(-accountAge)),
		username: name,
		member:   member,
		joinedAt: time.Now().Add(-joinAge),
	}
}

func setupPublicKeyTest(t *testing.T) *mockDiscord {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.PublicKeyClaim{}))
	model.ResetPoolLinkCache()
	t.Setenv("PUBLIC_POOL_DISCORD_CLIENT_ID", "test-client")
	t.Setenv("PUBLIC_POOL_DISCORD_CLIENT_SECRET", "test-secret")
	return newMockDiscord(t)
}

func createPoolForTest(t *testing.T, remain int) *model.Token {
	t.Helper()
	pool := &model.Token{
		UserId: 1, Key: "pool-" + common.GetRandomString(10), Name: "公益池",
		Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: remain, IsPublicPool: true,
	}
	require.NoError(t, pool.Insert())
	return pool
}

const (
	oldAccount = 400 * 24 * time.Hour
	oldMember  = 30 * 24 * time.Hour
)

func countChildren(t *testing.T, poolID int) int64 {
	t.Helper()
	var n int64
	require.NoError(t, model.DB.Model(&model.Token{}).Where("pool_parent_id = ?", poolID).Count(&n).Error)
	return n
}

func TestProcessDiscordClaimSuccessAndIdempotent(t *testing.T) {
	md := setupPublicKeyTest(t)
	pool := createPoolForTest(t, 10_000_000)
	md.addUser("alice", "alice", oldAccount, oldMember, true)

	ticket, errCode := processDiscordClaim(context.Background(), "alice", "http://x/cb", "10.0.0.1")
	require.Empty(t, errCode)
	assert.True(t, strings.HasPrefix(ticket.key, "sk-"))
	assert.False(t, ticket.existing)

	child, err := model.GetTokenById(1 + pool.Id)
	require.NoError(t, err)
	assert.Equal(t, pool.Id, child.PoolParentId)
	assert.Equal(t, strings.TrimPrefix(ticket.key, "sk-"), child.Key)
	assert.False(t, child.AllowUserQuotaQuery)

	// 同一个 Discord 账号换 IP 再来，拿到同一把 Key，不会新建。
	again, errCode := processDiscordClaim(context.Background(), "alice", "http://x/cb", "203.0.113.77")
	require.Empty(t, errCode)
	assert.True(t, again.existing)
	assert.Equal(t, ticket.key, again.key)
	assert.EqualValues(t, 1, countChildren(t, pool.Id))
}

func TestProcessDiscordClaimRejections(t *testing.T) {
	md := setupPublicKeyTest(t)
	pool := createPoolForTest(t, 10_000_000)
	md.addUser("stranger", "stranger", oldAccount, oldMember, false)
	md.addUser("newbie", "newbie", 2*24*time.Hour, oldMember, true)
	md.addUser("justjoined", "justjoined", oldAccount, time.Hour, true)

	cases := map[string]string{
		"stranger":   pkErrNotMember,
		"newbie":     pkErrAccountTooNew,
		"justjoined": pkErrJoinedTooRecent,
		"unknown":    pkErrDiscord,
	}
	for code, want := range cases {
		_, errCode := processDiscordClaim(context.Background(), code, "http://x/cb", "10.0.0.2")
		assert.Equal(t, want, errCode, code)
	}
	assert.EqualValues(t, 0, countChildren(t, pool.Id), "rejected visitors must not receive keys")
}

func TestProcessDiscordClaimBannedAccountCannotReclaim(t *testing.T) {
	md := setupPublicKeyTest(t)
	createPoolForTest(t, 10_000_000)
	md.addUser("mallory", "mallory", oldAccount, oldMember, true)

	_, errCode := processDiscordClaim(context.Background(), "mallory", "http://x/cb", "10.0.0.3")
	require.Empty(t, errCode)
	var claim model.PublicKeyClaim
	require.NoError(t, model.DB.First(&claim).Error)
	require.NoError(t, model.BanPublicKeyClaim(claim.Id))

	_, errCode = processDiscordClaim(context.Background(), "mallory", "http://x/cb", "198.51.100.9")
	assert.Equal(t, pkErrBanned, errCode)
}

func TestProcessDiscordClaimIPLimitAndNoPool(t *testing.T) {
	md := setupPublicKeyTest(t)
	t.Setenv("PUBLIC_POOL_CLAIMS_PER_IP_PER_DAY", "2")

	md.addUser("u1", "u1", oldAccount, oldMember, true)
	_, errCode := processDiscordClaim(context.Background(), "u1", "http://x/cb", "10.0.0.4")
	assert.Equal(t, pkErrNoPool, errCode, "no pool configured yet")

	createPoolForTest(t, 10_000_000)
	for i, name := range []string{"u1", "u2", "u3"} {
		md.addUser(name, name, oldAccount, oldMember, true)
		_, errCode := processDiscordClaim(context.Background(), name, "http://x/cb", "10.0.0.5")
		if i < 2 {
			assert.Empty(t, errCode, name)
		} else {
			assert.Equal(t, pkErrIPLimit, errCode, name)
		}
	}
}

func TestProcessDiscordClaimRespectsHourlyGlobalLimit(t *testing.T) {
	md := setupPublicKeyTest(t)
	t.Setenv("PUBLIC_POOL_MAX_CLAIMS_PER_HOUR", "1")
	createPoolForTest(t, 10_000_000)
	md.addUser("a", "a", oldAccount, oldMember, true)
	md.addUser("b", "b", oldAccount, oldMember, true)

	_, errCode := processDiscordClaim(context.Background(), "a", "http://x/cb", "10.0.1.1")
	require.Empty(t, errCode)
	_, errCode = processDiscordClaim(context.Background(), "b", "http://x/cb", "10.0.1.2")
	assert.Equal(t, pkErrBusy, errCode)
}

func TestOneTimeStoreCannotBeReused(t *testing.T) {
	s := newOneTimeStore[int](time.Minute)
	token, ok := s.put(7)
	require.True(t, ok)
	v, ok := s.take(token)
	assert.True(t, ok)
	assert.Equal(t, 7, v)
	_, ok = s.take(token)
	assert.False(t, ok, "second use must fail")

	_, ok = s.take("")
	assert.False(t, ok)

	expired := newOneTimeStore[int](-time.Second)
	token, _ = expired.put(1)
	_, ok = expired.take(token)
	assert.False(t, ok, "expired entries are rejected")
}

func publicKeyRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/config", GetPublicKeyConfig)
	r.GET("/start", PublicKeyDiscordStart)
	r.GET("/callback", PublicKeyDiscordCallback)
	r.POST("/redeem", PublicKeyRedeem)
	return r
}

func doRequest(r http.Handler, method, target, body, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("CF-Connecting-IP", ip)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPublicKeyHTTPFlowEndToEnd(t *testing.T) {
	md := setupPublicKeyTest(t)
	createPoolForTest(t, 10_000_000)
	md.addUser("bob", "bob", oldAccount, oldMember, true)
	r := publicKeyRouter()
	ip := "192.0.2.10"

	// 配置接口
	w := doRequest(r, http.MethodGet, "/config", "", ip)
	var cfg struct {
		Data struct {
			Available bool    `json:"available"`
			DailyCap  float64 `json:"daily_cap_usd"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cfg))
	assert.True(t, cfg.Data.Available)
	assert.InDelta(t, 10.0, cfg.Data.DailyCap, 0.001)

	// 发起授权，拿到带 state 的 Discord 地址
	w = doRequest(r, http.MethodGet, "/start", "", ip)
	var start struct {
		Success bool `json:"success"`
		Data    struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &start))
	require.True(t, start.Success)
	authURL, err := url.Parse(start.Data.URL)
	require.NoError(t, err)
	assert.Equal(t, "identify guilds.members.read", authURL.Query().Get("scope"))
	state := authURL.Query().Get("state")
	require.NotEmpty(t, state)

	// 回调：成功后重定向到 /balance#ticket=...
	w = doRequest(r, http.MethodGet, "/callback?code=bob&state="+state, "", ip)
	require.Equal(t, http.StatusFound, w.Code)
	loc := w.Header().Get("Location")
	require.True(t, strings.HasPrefix(loc, "/balance#ticket="), loc)
	ticket := strings.TrimPrefix(loc, "/balance#ticket=")

	// 同一个 state 不能再次使用
	w = doRequest(r, http.MethodGet, "/callback?code=bob&state="+state, "", ip)
	assert.Equal(t, "/balance#error="+pkErrStateInvalid, w.Header().Get("Location"))

	// 票据只能兑换一次
	w = doRequest(r, http.MethodPost, "/redeem", `{"ticket":"`+ticket+`"}`, ip)
	var redeem struct {
		Success bool `json:"success"`
		Data    struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &redeem))
	require.True(t, redeem.Success)
	assert.True(t, strings.HasPrefix(redeem.Data.Key, "sk-"))

	w = doRequest(r, http.MethodPost, "/redeem", `{"ticket":"`+ticket+`"}`, ip)
	assert.Contains(t, w.Body.String(), `"success":false`)
}

func TestPublicKeyCallbackDeniedAndForgedState(t *testing.T) {
	setupPublicKeyTest(t)
	createPoolForTest(t, 10_000_000)
	r := publicKeyRouter()

	w := doRequest(r, http.MethodGet, "/callback?code=x&state=forged", "", "192.0.2.20")
	assert.Equal(t, "/balance#error="+pkErrStateInvalid, w.Header().Get("Location"))

	w = doRequest(r, http.MethodGet, "/start", "", "192.0.2.21")
	var start struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &start))
	u, _ := url.Parse(start.Data.URL)
	w = doRequest(r, http.MethodGet, "/callback?error=access_denied&state="+u.Query().Get("state"), "", "192.0.2.21")
	assert.Equal(t, "/balance#error="+pkErrDenied, w.Header().Get("Location"))
}

func TestPublicKeyStartRateLimitedPerIP(t *testing.T) {
	setupPublicKeyTest(t)
	createPoolForTest(t, 10_000_000)
	r := publicKeyRouter()

	blocked := 0
	for i := 0; i < 14; i++ {
		w := doRequest(r, http.MethodGet, "/start", "", "192.0.2.30")
		if strings.Contains(w.Body.String(), pkErrRateLimited) {
			blocked++
		}
	}
	assert.Equal(t, 4, blocked)
}

func TestPublicKeyConfigUnavailableWithoutPoolOrDiscord(t *testing.T) {
	setupPublicKeyTest(t)
	r := publicKeyRouter()

	w := doRequest(r, http.MethodGet, "/config", "", "192.0.2.40")
	assert.Contains(t, w.Body.String(), `"reason":"`+pkErrNoPool+`"`)

	t.Setenv("PUBLIC_POOL_DISCORD_CLIENT_ID", "")
	t.Setenv("PUBLIC_POOL_DISCORD_CLIENT_SECRET", "")
	w = doRequest(r, http.MethodGet, "/config", "", "192.0.2.40")
	assert.Contains(t, w.Body.String(), `"reason":"`+pkErrDisabled+`"`)
}

func TestDiscordSnowflakeTime(t *testing.T) {
	want := time.Date(2020, 5, 17, 8, 30, 0, 0, time.UTC)
	got, ok := discordSnowflakeTime(makeSnowflake(want))
	require.True(t, ok)
	assert.WithinDuration(t, want, got, time.Second)
	_, ok = discordSnowflakeTime("not-a-number")
	assert.False(t, ok)
}

func TestValidatePublicPoolToggle(t *testing.T) {
	ctxWithRole := func(role int) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Set("role", role)
		return c
	}

	assert.False(t, validatePublicPoolToggle(ctxWithRole(common.RoleCommonUser), nil, false), "ordinary users cannot create public keys")
	assert.True(t, validatePublicPoolToggle(ctxWithRole(common.RoleAdminUser), nil, false))
	assert.False(t, validatePublicPoolToggle(ctxWithRole(common.RoleAdminUser), nil, true), "unlimited keys cannot be public")
	assert.False(t, validatePublicPoolToggle(ctxWithRole(common.RoleAdminUser), &model.Token{PoolParentId: 5}, false), "claimed keys cannot be public pools")
}

func TestBalanceUnlockRequiresTurnstile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/unlock", BalanceUnlock)

	old := common.TurnstileCheckEnabled
	t.Cleanup(func() { common.TurnstileCheckEnabled = old })

	common.TurnstileCheckEnabled = false
	w := doRequest(r, http.MethodPost, "/unlock", "", "203.0.113.50")
	assert.Contains(t, w.Body.String(), "turnstile_disabled", "without turnstile nobody may self-unlock")

	common.TurnstileCheckEnabled = true
	w = doRequest(r, http.MethodPost, "/unlock", "", "203.0.113.50")
	assert.Contains(t, w.Body.String(), `"success":true`)
}
