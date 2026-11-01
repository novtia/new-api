package middleware

import (
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// 余额页（/balance）查询接口的限流。
//
// 与全站“关键操作”限流分开计数：页面每次打开、查询都会请求接口，共用计数很快会触顶。
// 超限后访客可以通过 Cloudflare Turnstile 人机验证解锁：解锁后的 IP 在一段时间内使用更高的额度。
// 验证令牌由 Turnstile 校验保证，解锁记录只存在内存中，重启后需要重新验证。

const balanceQueryWindowSeconds int64 = 20 * 60

var (
	balanceLimiter = &common.InMemoryRateLimiter{}
	balanceUnlocks sync.Map // ip -> time.Time（解锁到期时间）
)

func init() {
	balanceLimiter.Init(30 * time.Minute)
}

func balanceUnlockTTL() time.Duration {
	return time.Duration(common.GetEnvOrDefault("BALANCE_UNLOCK_MINUTES", 60)) * time.Minute
}

// UnlockBalanceQueries 记录该 IP 已通过人机验证。
func UnlockBalanceQueries(ip string) time.Time {
	until := time.Now().Add(balanceUnlockTTL())
	balanceUnlocks.Store(ip, until)
	return until
}

func balanceUnlocked(ip string) bool {
	v, ok := balanceUnlocks.Load(ip)
	if !ok {
		return false
	}
	if time.Now().After(v.(time.Time)) {
		balanceUnlocks.Delete(ip)
		return false
	}
	return true
}

// ResetBalanceRateLimitForTest 清空限流与解锁状态，仅供测试使用。
func ResetBalanceRateLimitForTest() {
	balanceUnlocks.Range(func(k, _ any) bool {
		balanceUnlocks.Delete(k)
		return true
	})
	balanceLimiter = &common.InMemoryRateLimiter{}
	balanceLimiter.Init(30 * time.Minute)
}

// BalanceQueryRateLimit 限制余额、调用记录查询。未验证的 IP 使用基础额度，
// 通过人机验证的 IP 使用更高的额度。超限返回 429。
func BalanceQueryRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := common.RealClientIP(c)
		limit := common.GetEnvOrDefault("BALANCE_QUERY_LIMIT", 20)
		key := "BQ:" + ip
		if balanceUnlocked(ip) {
			limit = common.GetEnvOrDefault("BALANCE_QUERY_LIMIT_VERIFIED", 120)
			key = "BQV:" + ip
		}
		if !balanceLimiter.Request(key, limit, balanceQueryWindowSeconds) {
			writeRateLimited(c, balanceQueryWindowSeconds)
			return
		}
	}
}
