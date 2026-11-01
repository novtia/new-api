package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func balanceRequest(r http.Handler, ip string) int {
	req := httptest.NewRequest(http.MethodGet, "/q", nil)
	req.Header.Set("CF-Connecting-IP", ip)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestBalanceQueryRateLimitAndUnlock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ResetBalanceRateLimitForTest()
	t.Setenv("BALANCE_QUERY_LIMIT", "3")
	t.Setenv("BALANCE_QUERY_LIMIT_VERIFIED", "5")

	r := gin.New()
	r.GET("/q", BalanceQueryRateLimit(), func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < 3; i++ {
		assert.Equal(t, http.StatusOK, balanceRequest(r, "198.51.100.1"))
	}
	assert.Equal(t, http.StatusTooManyRequests, balanceRequest(r, "198.51.100.1"))
	// 不同访客（Cloudflare 传入的真实 IP 不同）互不影响。
	assert.Equal(t, http.StatusOK, balanceRequest(r, "198.51.100.2"))

	// 通过人机验证后恢复，并获得更高额度。
	UnlockBalanceQueries("198.51.100.1")
	for i := 0; i < 5; i++ {
		assert.Equal(t, http.StatusOK, balanceRequest(r, "198.51.100.1"))
	}
	assert.Equal(t, http.StatusTooManyRequests, balanceRequest(r, "198.51.100.1"))
}
