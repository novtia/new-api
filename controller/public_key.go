package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

// 公益密钥领取流程
//
//  1. 访客在 /balance 通过 Turnstile，调用 /api/public_key/discord/start 获得 Discord 授权地址；
//  2. Discord 授权后回调 /api/public_key/discord/callback：校验 state、账号年龄、
//     是否在指定服务器及入群时长，通过后创建（或找回）该 Discord 账号唯一的访客 Key；
//  3. 回调重定向到 /balance#ticket=...，前端用一次性票据调用 /api/public_key/redeem 取回 Key。
//
// “一个 Discord 账号一个 Key”由 public_key_claims.discord_id 的唯一索引保证，
// 换 IP、换设备都无法绕开。IP 和全站频率限制只是辅助的风控层。

const (
	defaultPoolGuildID = "1134557553011998840" // https://discord.gg/odysseia

	publicKeyStateTTL  = 10 * time.Minute
	publicKeyTicketTTL = 5 * time.Minute
	publicKeyStoreMax  = 20000
)

// 以下变量在测试中被替换为本地 mock 服务。
var (
	discordAPIBase      = "https://discord.com/api/v10"
	discordAuthorizeURL = "https://discord.com/oauth2/authorize"
)

// 领取失败的错误码，前端据此展示中文提示。
const (
	pkErrDisabled        = "disabled"
	pkErrNoPool          = "no_pool"
	pkErrRateLimited     = "rate_limited"
	pkErrStateInvalid    = "state_invalid"
	pkErrDenied          = "denied"
	pkErrDiscord         = "discord_error"
	pkErrNotMember       = "not_member"
	pkErrAccountTooNew   = "account_too_new"
	pkErrJoinedTooRecent = "joined_too_recent"
	pkErrBanned          = "banned"
	pkErrIPLimit         = "ip_limit"
	pkErrBusy            = "busy"
	pkErrInternal        = "internal"
)

var publicKeyLimiter common.InMemoryRateLimiter

func init() {
	publicKeyLimiter.Init(20 * time.Minute)
}

// oneTimeStore 是带过期时间的一次性令牌存储（state 与票据共用）。
type oneTimeStore[T any] struct {
	mu  sync.Mutex
	m   map[string]oneTimeEntry[T]
	ttl time.Duration
}

type oneTimeEntry[T any] struct {
	value   T
	expires time.Time
}

func newOneTimeStore[T any](ttl time.Duration) *oneTimeStore[T] {
	return &oneTimeStore[T]{m: make(map[string]oneTimeEntry[T]), ttl: ttl}
}

func (s *oneTimeStore[T]) sweepLocked(now time.Time) {
	for k, e := range s.m {
		if now.After(e.expires) {
			delete(s.m, k)
		}
	}
}

func (s *oneTimeStore[T]) put(v T) (string, bool) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false
	}
	token := hex.EncodeToString(buf)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= publicKeyStoreMax {
		s.sweepLocked(now)
		if len(s.m) >= publicKeyStoreMax {
			return "", false
		}
	}
	s.m[token] = oneTimeEntry[T]{value: v, expires: now.Add(s.ttl)}
	return token, true
}

// take 取出并删除，保证同一个 state/票据只能使用一次。
func (s *oneTimeStore[T]) take(token string) (T, bool) {
	var zero T
	if token == "" {
		return zero, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[token]
	if !ok {
		return zero, false
	}
	delete(s.m, token)
	if time.Now().After(e.expires) {
		return zero, false
	}
	return e.value, true
}

type publicKeyState struct {
	ip string
}

type publicKeyTicket struct {
	key         string
	discordName string
	existing    bool
	dailyCap    int // 该访客 Key 的每日上限（额度单位）
	dailyCount  int // 每日请求次数上限，0 表示不限制
	dailyImages int // 每日图片张数上限，0 表示不限制
}

var (
	publicKeyStates  = newOneTimeStore[publicKeyState](publicKeyStateTTL)
	publicKeyTickets = newOneTimeStore[publicKeyTicket](publicKeyTicketTTL)
)

func poolGuildID() string {
	return common.GetEnvOrDefaultString("PUBLIC_POOL_DISCORD_GUILD_ID", defaultPoolGuildID)
}

func poolDiscordClient() (id, secret string) {
	id = common.GetEnvOrDefaultString("PUBLIC_POOL_DISCORD_CLIENT_ID", "")
	secret = common.GetEnvOrDefaultString("PUBLIC_POOL_DISCORD_CLIENT_SECRET", "")
	if id == "" || secret == "" {
		s := system_setting.GetDiscordSettings()
		id, secret = s.ClientId, s.ClientSecret
	}
	return id, secret
}

func poolRedirectURI(c *gin.Context) string {
	if v := common.GetEnvOrDefaultString("PUBLIC_POOL_REDIRECT_URI", ""); v != "" {
		return v
	}
	base := strings.TrimRight(system_setting.ServerAddress, "/")
	if base == "" {
		scheme := "https"
		if p := c.GetHeader("X-Forwarded-Proto"); p != "" {
			scheme = p
		} else if c.Request.TLS == nil {
			scheme = "http"
		}
		base = scheme + "://" + c.Request.Host
	}
	return base + "/api/public_key/discord/callback"
}

// publicKeyClientIP 优先使用 Cloudflare 传入的真实访客 IP。
func publicKeyClientIP(c *gin.Context) string {
	return common.RealClientIP(c)
}

// BalanceUnlock 在 Turnstile 校验通过后，解除该访客 IP 的余额查询限流（提高额度）。
// 路由上必须先经过 TurnstileCheck；而 TurnstileCheck 在未启用时会直接放行，
// 所以这里必须再确认 Turnstile 已启用，否则任何人都能免费解锁。
func BalanceUnlock(c *gin.Context) {
	if !common.TurnstileCheckEnabled {
		c.JSON(http.StatusOK, gin.H{"success": false, "code": "turnstile_disabled", "message": "turnstile_disabled"})
		return
	}
	until := middleware.UnlockBalanceQueries(publicKeyClientIP(c))
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"unlocked_until": until.Unix()}})
}

func publicKeyAvailable() (clientID string, errCode string) {
	id, secret := poolDiscordClient()
	if id == "" || secret == "" {
		return "", pkErrDisabled
	}
	if _, err := model.PickPublicPool(); err != nil {
		return "", pkErrNoPool
	}
	return id, ""
}

func quotaToUSD(q int) float64 {
	return float64(q) / common.QuotaPerUnit
}

// GetPublicKeyConfig 返回 /balance 页面渲染领取区块所需的公开配置。
func GetPublicKeyConfig(c *gin.Context) {
	_, errCode := publicKeyAvailable()
	dailyCap := model.PublicPoolDailyCapQuota()
	var dailyCount, dailyImages int
	if pool, err := model.PickPublicPool(); err == nil {
		dailyCap, _ = model.EffectivePoolCaps(pool)
		dailyCount, dailyImages = model.EffectivePoolCountCaps(pool)
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"available":          errCode == "",
			"reason":             errCode,
			"turnstile_enabled":  common.TurnstileCheckEnabled,
			"turnstile_site_key": common.TurnstileSiteKey,
			"daily_cap_usd":      quotaToUSD(dailyCap),
			"daily_cap_count":    dailyCount,
			"daily_cap_images":   dailyImages,
		},
	})
}

// PublicKeyDiscordStart 在 Turnstile 通过后签发 state 并返回 Discord 授权地址。
func PublicKeyDiscordStart(c *gin.Context) {
	ip := publicKeyClientIP(c)
	if !publicKeyLimiter.Request("pk-start:"+ip, 10, 600) {
		publicKeyFail(c, pkErrRateLimited)
		return
	}
	clientID, errCode := publicKeyAvailable()
	if errCode != "" {
		publicKeyFail(c, errCode)
		return
	}
	state, ok := publicKeyStates.put(publicKeyState{ip: ip})
	if !ok {
		publicKeyFail(c, pkErrBusy)
		return
	}
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", poolRedirectURI(c))
	q.Set("response_type", "code")
	q.Set("scope", "identify guilds.members.read")
	q.Set("state", state)
	q.Set("prompt", "consent")
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"url": discordAuthorizeURL + "?" + q.Encode()},
	})
}

func publicKeyFail(c *gin.Context, code string) {
	c.JSON(http.StatusOK, gin.H{"success": false, "message": code, "code": code})
}

// PublicKeyDiscordCallback 处理 Discord 回调，并把结果以 URL 片段形式带回 /balance。
func PublicKeyDiscordCallback(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")

	ip := publicKeyClientIP(c)
	if !publicKeyLimiter.Request("pk-cb:"+ip, 20, 600) {
		publicKeyRedirect(c, "error", pkErrRateLimited)
		return
	}
	if c.Query("error") != "" {
		publicKeyStates.take(c.Query("state"))
		publicKeyRedirect(c, "error", pkErrDenied)
		return
	}
	state, ok := publicKeyStates.take(c.Query("state"))
	if !ok {
		publicKeyRedirect(c, "error", pkErrStateInvalid)
		return
	}
	_ = state

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	ticket, errCode := processDiscordClaim(ctx, c.Query("code"), poolRedirectURI(c), ip)
	if errCode != "" {
		publicKeyRedirect(c, "error", errCode)
		return
	}
	token, ok := publicKeyTickets.put(ticket)
	if !ok {
		publicKeyRedirect(c, "error", pkErrBusy)
		return
	}
	publicKeyRedirect(c, "ticket", token)
}

func publicKeyRedirect(c *gin.Context, name, value string) {
	c.Redirect(http.StatusFound, "/balance#"+name+"="+url.QueryEscape(value))
}

type discordIdentity struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Global   string `json:"global_name"`
}

func (u discordIdentity) displayName() string {
	name := u.Global
	if name == "" {
		name = u.Username
	}
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "user"
	}
	return b.String()
}

var errDiscordUnavailable = errors.New("discord request failed")

func discordGet(ctx context.Context, accessToken, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discordAPIBase+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	res, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errDiscordUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK && out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			return res.StatusCode, err
		}
	}
	return res.StatusCode, nil
}

func discordExchangeCode(ctx context.Context, code, redirectURI string) (string, error) {
	clientID, secret := poolDiscordClient()
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", secret)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, discordAPIBase+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errDiscordUnavailable, err)
	}
	defer res.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.AccessToken == "" {
		return "", errors.New("discord token exchange failed")
	}
	return body.AccessToken, nil
}

// discordSnowflakeTime 从 Discord ID（雪花）中还原账号创建时间。
func discordSnowflakeTime(id string) (time.Time, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}, false
	}
	ms := int64(n>>22) + 1420070400000
	return time.UnixMilli(ms), true
}

// processDiscordClaim 完成 Discord 身份校验并创建或找回访客 Key。
// 返回票据内容；失败时返回错误码。
func processDiscordClaim(ctx context.Context, code, redirectURI, ip string) (publicKeyTicket, string) {
	var zero publicKeyTicket
	if code == "" {
		return zero, pkErrDiscord
	}
	if _, errCode := publicKeyAvailable(); errCode != "" {
		return zero, errCode
	}

	accessToken, err := discordExchangeCode(ctx, code, redirectURI)
	if err != nil {
		common.SysLog("public key: discord token exchange failed: " + err.Error())
		return zero, pkErrDiscord
	}

	var user discordIdentity
	if status, err := discordGet(ctx, accessToken, "/users/@me", &user); err != nil || status != http.StatusOK || user.ID == "" {
		return zero, pkErrDiscord
	}

	// 账号年龄：新注册的小号一律拒绝。
	if minDays := common.GetEnvOrDefault("PUBLIC_POOL_MIN_ACCOUNT_AGE_DAYS", 7); minDays > 0 {
		created, ok := discordSnowflakeTime(user.ID)
		if !ok {
			return zero, pkErrDiscord
		}
		if time.Since(created) < time.Duration(minDays)*24*time.Hour {
			return zero, pkErrAccountTooNew
		}
	}

	// 必须是指定服务器成员，且入群满一定天数。
	var member struct {
		JoinedAt string `json:"joined_at"`
	}
	status, err := discordGet(ctx, accessToken, "/users/@me/guilds/"+poolGuildID()+"/member", &member)
	if err != nil {
		return zero, pkErrDiscord
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return zero, pkErrNotMember
	default:
		return zero, pkErrDiscord
	}
	if minDays := common.GetEnvOrDefault("PUBLIC_POOL_MIN_JOIN_AGE_DAYS", 3); minDays > 0 {
		joined, err := time.Parse(time.RFC3339Nano, member.JoinedAt)
		if err != nil {
			return zero, pkErrDiscord
		}
		if time.Since(joined) < time.Duration(minDays)*24*time.Hour {
			return zero, pkErrJoinedTooRecent
		}
	}

	return claimPublicKey(user, ip)
}

// claimPublicKey 为已通过校验的 Discord 账号创建或找回访客 Key。
func claimPublicKey(user discordIdentity, ip string) (publicKeyTicket, string) {
	var zero publicKeyTicket

	if existing, err := model.GetPublicKeyClaimByDiscordID(user.ID); err == nil {
		return ticketForExistingClaim(existing, user)
	}

	// 辅助风控：同一 IP 每天最多领取若干个账号，全站每小时新增数量设上限。
	now := common.GetTimestamp()
	if n, err := model.CountPublicKeyClaimsByIPSince(ip, now-86400); err != nil {
		return zero, pkErrInternal
	} else if int(n) >= common.GetEnvOrDefault("PUBLIC_POOL_CLAIMS_PER_IP_PER_DAY", 3) {
		return zero, pkErrIPLimit
	}
	if n, err := model.CountPublicKeyClaimsSince(now - 3600); err != nil {
		return zero, pkErrInternal
	} else if int(n) >= common.GetEnvOrDefault("PUBLIC_POOL_MAX_CLAIMS_PER_HOUR", 30) {
		return zero, pkErrBusy
	}

	pool, err := model.PickPublicPool()
	if err != nil {
		return zero, pkErrNoPool
	}
	key, err := common.GenerateKey()
	if err != nil {
		return zero, pkErrInternal
	}
	claim := &model.PublicKeyClaim{DiscordId: user.ID, DiscordName: user.displayName(), ClientIp: ip}
	child, err := model.CreatePublicKeyClaim(pool, claim, key)
	if errors.Is(err, model.ErrPublicKeyAlreadyClaimed) {
		// 并发领取：另一个请求已经成功，直接返回它创建的 Key。
		if existing, getErr := model.GetPublicKeyClaimByDiscordID(user.ID); getErr == nil {
			return ticketForExistingClaim(existing, user)
		}
		return zero, pkErrInternal
	}
	if err != nil {
		common.SysError("public key: failed to create claim: " + err.Error())
		return zero, pkErrInternal
	}
	common.SysLog(fmt.Sprintf("public key: discord %s claimed token %d from pool %d (ip %s)", user.ID, child.Id, pool.Id, ip))
	return publicKeyTicket{key: "sk-" + child.GetFullKey(), discordName: claim.DiscordName, dailyCap: child.DailyCapQuota, dailyCount: child.DailyCapCount, dailyImages: child.DailyCapImages}, ""
}

func ticketForExistingClaim(existing *model.PublicKeyClaim, user discordIdentity) (publicKeyTicket, string) {
	if existing.Banned {
		return publicKeyTicket{}, pkErrBanned
	}
	token, err := model.GetTokenById(existing.TokenId)
	if err != nil {
		return publicKeyTicket{}, pkErrInternal
	}
	return publicKeyTicket{key: "sk-" + token.GetFullKey(), discordName: existing.DiscordName, existing: true, dailyCap: token.DailyCapQuota, dailyCount: token.DailyCapCount, dailyImages: token.DailyCapImages}, ""
}

// PublicKeyRedeem 用一次性票据换取 Key。
func PublicKeyRedeem(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ip := publicKeyClientIP(c)
	if !publicKeyLimiter.Request("pk-redeem:"+ip, 30, 600) {
		publicKeyFail(c, pkErrRateLimited)
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
	}
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		publicKeyFail(c, pkErrStateInvalid)
		return
	}
	ticket, ok := publicKeyTickets.take(req.Ticket)
	if !ok {
		publicKeyFail(c, pkErrStateInvalid)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"key":              ticket.key,
			"discord_name":     ticket.discordName,
			"existing":         ticket.existing,
			"daily_cap_usd":    quotaToUSD(ticket.dailyCap),
			"daily_cap_count":  ticket.dailyCount,
			"daily_cap_images": ticket.dailyImages,
		},
	})
}

// ListPublicKeyClaims 后台查看领取记录（管理员）。
func ListPublicKeyClaims(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("p", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	items, total, err := model.ListPublicKeyClaims((page-1)*size, size)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"items": items, "total": total, "page": page, "page_size": size},
	})
}

// BanPublicKeyClaim 封禁某个领取记录并停用其 Key（管理员）。
func BanPublicKeyClaim(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "invalid id")
		return
	}
	if err := model.BanPublicKeyClaim(id); err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": ""})
}
