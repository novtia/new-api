package model

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupPoolTest(t *testing.T) {
	t.Helper()
	truncateTables(t)
	resetBatchUpdateTestState(t)
	require.NoError(t, DB.AutoMigrate(&PublicKeyClaim{}))
	ResetPoolLinkCache()
	t.Cleanup(func() {
		DB.Exec("DELETE FROM public_key_claims")
		ResetPoolLinkCache()
	})
}

func createTestPool(t *testing.T, remain int) Token {
	t.Helper()
	token := Token{
		UserId:       1,
		Key:          "pool-parent-" + common.GetRandomString(8),
		Name:         "pool",
		Status:       common.TokenStatusEnabled,
		ExpiredTime:  -1,
		RemainQuota:  remain,
		IsPublicPool: true,
	}
	require.NoError(t, token.Insert())
	return token
}

func createTestPoolChild(t *testing.T, pool Token, dailyCap, hourlyCap int) Token {
	t.Helper()
	token := Token{
		UserId:         1,
		Key:            "pool-child-" + common.GetRandomString(8),
		Name:           "child",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
		PoolParentId:   pool.Id,
		DailyCapQuota:  dailyCap,
		HourlyCapQuota: hourlyCap,
	}
	require.NoError(t, token.Insert())
	return token
}

func TestPublicPoolCallAllowsOneInFlight(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 100_000, 0)
	other := createTestPoolChild(t, pool, 100_000, 0)

	release, ok := AcquirePublicPoolCall(child.Id)
	require.True(t, ok)
	require.NotNil(t, release)
	t.Cleanup(release)

	_, busy := AcquirePublicPoolCall(child.Id)
	assert.False(t, busy)

	releaseOther, ok := AcquirePublicPoolCall(other.Id)
	require.True(t, ok)
	releaseOther()

	// 公益池本身不是访客 Key，不占这个名额。
	releaseParent, ok := AcquirePublicPoolCall(pool.Id)
	require.True(t, ok)
	releaseParentAgain, ok := AcquirePublicPoolCall(pool.Id)
	require.True(t, ok)
	releaseParent()
	releaseParentAgain()

	release()
	release, ok = AcquirePublicPoolCall(child.Id)
	require.True(t, ok)
	release()
}

func TestPublicPoolCallRejectsOverlappingAttempts(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 100_000, 0)

	release, ok := AcquirePublicPoolCall(child.Id)
	require.True(t, ok)
	t.Cleanup(release)

	var failed atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, acquired := AcquirePublicPoolCall(child.Id); !acquired {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(16), failed.Load())
}

func TestPoolChildChargesParentThroughReserveSettleRefund(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 100_000, 0)

	reserved, err := TryReserveTokenQuota(child.Id, child.Key, 100, true)
	require.NoError(t, err)
	require.True(t, reserved)

	got := getTokenFromDB(t, child.Id)
	assert.Equal(t, 100, got.UsedQuota)
	assert.Equal(t, 100, got.DailyUsedQuota)
	assert.Equal(t, 1_000_000-100, getTokenFromDB(t, pool.Id).RemainQuota)

	// 实际用量比预扣多 50：补扣同样落在父密钥上。
	require.NoError(t, DecreaseTokenQuota(child.Id, child.Key, 50))
	assert.Equal(t, 1_000_000-150, getTokenFromDB(t, pool.Id).RemainQuota)
	assert.Equal(t, 150, getTokenFromDB(t, child.Id).DailyUsedQuota)

	// 退款 30：父密钥与窗口计数同步回退。
	require.NoError(t, IncreaseTokenQuota(child.Id, child.Key, 30))
	assert.Equal(t, 1_000_000-120, getTokenFromDB(t, pool.Id).RemainQuota)
	got = getTokenFromDB(t, child.Id)
	assert.Equal(t, 120, got.DailyUsedQuota)
	assert.Equal(t, 120, got.UsedQuota)
}

func TestPoolChildDailyCapBlocksAndLeavesParentUntouched(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 1000, 0)

	reserved, err := TryReserveTokenQuota(child.Id, child.Key, 600, true)
	require.NoError(t, err)
	require.True(t, reserved)

	reserved, err = TryReserveTokenQuota(child.Id, child.Key, 600, true)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrPublicPoolDailyLimit)

	assert.Equal(t, 1_000_000-600, getTokenFromDB(t, pool.Id).RemainQuota, "failed reserve must not charge the pool")
	assert.Equal(t, 600, getTokenFromDB(t, child.Id).DailyUsedQuota)
}

func TestPoolChildDailyWindowRollsOver(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 1000, 0)

	// 模拟昨天已经用满。
	yesterday := PoolDayKey(time.Now().Add(-24 * time.Hour))
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", child.Id).
		Updates(map[string]any{"daily_used_quota": 1000, "daily_reset_day": yesterday}).Error)

	reserved, err := TryReserveTokenQuota(child.Id, child.Key, 400, true)
	require.NoError(t, err)
	require.True(t, reserved)
	got := getTokenFromDB(t, child.Id)
	assert.Equal(t, 400, got.DailyUsedQuota)
	assert.Equal(t, PoolDayKey(time.Now()), got.DailyResetDay)
}

func TestPoolChildHourlyCapFlagsOnlyRealBursts(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 10_000, 400)

	// 单次预估就超过小时上限：拒绝但不标记。
	reserved, err := TryReserveTokenQuota(child.Id, child.Key, 500, true)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrPublicPoolHourlyLim)
	assert.Zero(t, getTokenFromDB(t, child.Id).PoolFlaggedAt)

	reserved, err = TryReserveTokenQuota(child.Id, child.Key, 300, true)
	require.NoError(t, err)
	require.True(t, reserved)

	// 已经消耗后再冲破小时上限：拒绝并标记。
	reserved, err = TryReserveTokenQuota(child.Id, child.Key, 200, true)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrPublicPoolHourlyLim)
	assert.NotZero(t, getTokenFromDB(t, child.Id).PoolFlaggedAt)
}

func TestPoolExhaustionInvalidatesChildren(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 50)
	child := createTestPoolChild(t, pool, 100_000, 0)

	reserved, err := TryReserveTokenQuota(child.Id, child.Key, 100, true)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrPublicPoolExhausted)
	assert.Zero(t, getTokenFromDB(t, child.Id).DailyUsedQuota, "window usage must be rolled back")
	assert.Zero(t, getTokenFromDB(t, child.Id).UsedQuota)

	// 池子还有额度时鉴权通过。
	_, err = ValidateUserToken(child.Key)
	require.NoError(t, err)

	// 池子耗尽后访客 Key 鉴权失败。
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", pool.Id).Update("remain_quota", 0).Error)
	_, err = ValidateUserToken(child.Key)
	assert.ErrorIs(t, err, ErrTokenInvalid)

	// 管理员取消公益标记同样让访客 Key 失效。
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", pool.Id).
		Updates(map[string]any{"remain_quota": 1000, "is_public_pool": false}).Error)
	_, err = ValidateUserToken(child.Key)
	assert.ErrorIs(t, err, ErrTokenInvalid)
}

func TestPoolChildRejectedAtAuthWhenWindowIsFull(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 1000, 0)

	reserved, err := TryReserveTokenQuota(child.Id, child.Key, 1000, true)
	require.NoError(t, err)
	require.True(t, reserved)

	_, err = ValidateUserToken(child.Key)
	assert.ErrorIs(t, err, ErrTokenInvalid)
}

func TestPoolConcurrentReservesNeverOverspendPool(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1000)
	child := createTestPoolChild(t, pool, 1_000_000, 0)

	var ok int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reserved, _ := TryReserveTokenQuota(child.Id, child.Key, 100, true)
			if reserved {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()

	assert.EqualValues(t, 10, ok)
	assert.Equal(t, 0, getTokenFromDB(t, pool.Id).RemainQuota)
	assert.Equal(t, 1000, getTokenFromDB(t, child.Id).DailyUsedQuota)
}

func TestOrdinaryTokensAreUnaffectedByPoolHooks(t *testing.T) {
	setupPoolTest(t)
	token := createReserveTestToken(t, 500)

	reserved, err := TryReserveTokenQuota(token.Id, token.Key, 200, false)
	require.NoError(t, err)
	require.True(t, reserved)
	require.NoError(t, IncreaseTokenQuota(token.Id, token.Key, 50))
	assert.Equal(t, 350, getTokenFromDB(t, token.Id).RemainQuota)
	assert.False(t, IsPublicPoolChild(token.Id))
}

func TestCreatePublicKeyClaimIsOncePerDiscordAccount(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)

	claim := func() *PublicKeyClaim {
		return &PublicKeyClaim{DiscordId: "1234567890", DiscordName: "tester", ClientIp: "1.2.3.4"}
	}
	child, err := CreatePublicKeyClaim(&pool, claim(), "sk-first-"+common.GetRandomString(8))
	require.NoError(t, err)
	assert.True(t, child.UnlimitedQuota)
	assert.False(t, child.AllowUserQuotaQuery)
	assert.Equal(t, pool.Id, child.PoolParentId)
	assert.Equal(t, PublicPoolDailyCapQuota(), child.DailyCapQuota)

	_, err = CreatePublicKeyClaim(&pool, claim(), "sk-second-"+common.GetRandomString(8))
	assert.ErrorIs(t, err, ErrPublicKeyAlreadyClaimed)

	var count int64
	require.NoError(t, DB.Model(&Token{}).Where("pool_parent_id = ?", pool.Id).Count(&count).Error)
	assert.EqualValues(t, 1, count, "the failed claim must roll its token back")
}

func TestCreatePublicKeyClaimConcurrentDuplicates(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)

	var success int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &PublicKeyClaim{DiscordId: "999000111", DiscordName: "racer", ClientIp: "5.6.7.8"}
			if _, err := CreatePublicKeyClaim(&pool, c, "sk-race-"+common.GetRandomString(10)); err == nil {
				atomic.AddInt32(&success, 1)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, success)

	var count int64
	require.NoError(t, DB.Model(&Token{}).Where("pool_parent_id = ?", pool.Id).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestPickPublicPoolSkipsUnusablePools(t *testing.T) {
	setupPoolTest(t)
	_, err := PickPublicPool()
	assert.ErrorIs(t, err, ErrNoPublicPool)

	empty := createTestPool(t, 0)
	_ = empty
	_, err = PickPublicPool()
	assert.ErrorIs(t, err, ErrNoPublicPool)

	good := createTestPool(t, 5000)
	picked, err := PickPublicPool()
	require.NoError(t, err)
	assert.Equal(t, good.Id, picked.Id)
}

func TestBanPublicKeyClaimDisablesToken(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	claim := &PublicKeyClaim{DiscordId: "42424242", DiscordName: "bad", ClientIp: "9.9.9.9"}
	child, err := CreatePublicKeyClaim(&pool, claim, "sk-ban-"+common.GetRandomString(8))
	require.NoError(t, err)

	require.NoError(t, BanPublicKeyClaim(claim.Id))
	assert.Equal(t, common.TokenStatusDisabled, getTokenFromDB(t, child.Id).Status)
	got, err := GetPublicKeyClaimByDiscordID("42424242")
	require.NoError(t, err)
	assert.True(t, got.Banned)

	_, err = ValidateUserToken(child.Key)
	assert.ErrorIs(t, err, ErrTokenInvalid)
}

func TestPoolDayKeyUsesShanghaiTime(t *testing.T) {
	// UTC 2026-10-14 16:30 已经是东八区 10-15 00:30。
	utc := time.Date(2026, 10, 14, 16, 30, 0, 0, time.UTC)
	assert.Equal(t, 20261015, PoolDayKey(utc))
	assert.Equal(t, time.Date(2026, 10, 15, 16, 0, 0, 0, time.UTC).Unix(), PoolNextResetAt(utc))
}

func TestPoolLimitsAreConfigurableAndPropagateToExistingChildren(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 10_000_000)

	// 未设置时使用默认值：每日默认上限，每小时为其默认比例。
	daily, hourly := EffectivePoolCaps(&pool)
	assert.Equal(t, PublicPoolDailyCapQuota(), daily)
	assert.Equal(t, PublicPoolHourlyCapQuota(), hourly)

	first, err := CreatePublicKeyClaim(&pool, &PublicKeyClaim{DiscordId: "1001", DiscordName: "a", ClientIp: "1.1.1.1"}, "sk-a-"+common.GetRandomString(8))
	require.NoError(t, err)
	assert.Equal(t, daily, first.DailyCapQuota)

	// 管理员把限额改成每日 3000 / 每小时 500（额度单位）。
	pool.DailyCapQuota, pool.HourlyCapQuota = 3000, 500
	require.NoError(t, pool.Update())
	require.NoError(t, PropagatePoolLimits(&pool))

	got := getTokenFromDB(t, first.Id)
	assert.Equal(t, 3000, got.DailyCapQuota, "existing visitors follow the new limit")
	assert.Equal(t, 500, got.HourlyCapQuota)

	reloaded := getTokenFromDB(t, pool.Id)
	second, err := CreatePublicKeyClaim(&reloaded, &PublicKeyClaim{DiscordId: "1002", DiscordName: "b", ClientIp: "2.2.2.2"}, "sk-b-"+common.GetRandomString(8))
	require.NoError(t, err)
	assert.Equal(t, 3000, second.DailyCapQuota)
	assert.Equal(t, 500, second.HourlyCapQuota)

	// 只设置每日、每小时留空：每小时按新的每日上限折算。
	reloaded.DailyCapQuota, reloaded.HourlyCapQuota = 5000, 0
	d, h := EffectivePoolCaps(&reloaded)
	assert.Equal(t, 5000, d)
	assert.Equal(t, 5000*poolHourlyPercent()/100, h)

	// 新限额真实生效：每小时 500，占用 600 会被拦。
	reserved, err := TryReserveTokenQuota(first.Id, first.Key, 600, true)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrPublicPoolHourlyLim)
}

func setPoolChildCountCaps(t *testing.T, child Token, requests, images int) {
	t.Helper()
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", child.Id).
		Updates(map[string]any{"daily_cap_count": requests, "daily_cap_images": images}).Error)
	ResetPoolLinkCache()
}

func TestPoolChildCountAndImageCapsAreIndependentOfQuota(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 0, 0)
	setPoolChildCountCaps(t, child, 3, 4)

	for range 2 {
		reserved, err := ReservePoolChildUsage(child.Id, 1, 2)
		require.NoError(t, err)
		require.True(t, reserved)
	}
	got := getTokenFromDB(t, child.Id)
	assert.Equal(t, 2, got.DailyUsedCount)
	assert.Equal(t, 4, got.DailyUsedImages)
	assert.Equal(t, 1_000_000, getTokenFromDB(t, pool.Id).RemainQuota, "count limits must not touch the pool quota")

	// 张数已满：次数还有余量也要拒绝，且不留下任何占用。
	reserved, err := ReservePoolChildUsage(child.Id, 1, 1)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrPublicPoolDailyImageLimit)
	got = getTokenFromDB(t, child.Id)
	assert.Equal(t, 2, got.DailyUsedCount)
	assert.Equal(t, 4, got.DailyUsedImages)

	// 退还一次请求后可以再用；多退不会变成负数。
	RefundPoolChildUsage(child.Id, 1, 2)
	reserved, err = ReservePoolChildUsage(child.Id, 1, 2)
	require.NoError(t, err)
	assert.True(t, reserved)
	RefundPoolChildUsage(child.Id, 10, 10)
	got = getTokenFromDB(t, child.Id)
	assert.Equal(t, 0, got.DailyUsedCount)
	assert.Equal(t, 0, got.DailyUsedImages)
}

func TestPoolChildCountCapsRollOverAndBlockAtAuth(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 0, 0)
	setPoolChildCountCaps(t, child, 2, 0)

	for range 2 {
		reserved, err := ReservePoolChildUsage(child.Id, 1, 0)
		require.NoError(t, err)
		require.True(t, reserved)
	}
	_, err := ValidateUserToken(child.Key)
	assert.Error(t, err, "a visitor at its request cap is rejected before reaching the upstream")

	yesterday := PoolDayKey(time.Now().Add(-24 * time.Hour))
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", child.Id).Update("daily_reset_day", yesterday).Error)
	ResetPoolLinkCache()
	_, err = ValidateUserToken(child.Key)
	require.NoError(t, err)
	reserved, err := ReservePoolChildUsage(child.Id, 1, 0)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, 1, getTokenFromDB(t, child.Id).DailyUsedCount, "a new day starts from zero")
}

func TestPoolChildConcurrentCountReservesNeverExceedCap(t *testing.T) {
	setupPoolTest(t)
	pool := createTestPool(t, 1_000_000)
	child := createTestPoolChild(t, pool, 0, 0)
	setPoolChildCountCaps(t, child, 10, 0)

	var ok int32
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			if reserved, _ := ReservePoolChildUsage(child.Id, 1, 0); reserved {
				atomic.AddInt32(&ok, 1)
			}
		})
	}
	wg.Wait()

	assert.EqualValues(t, 10, ok)
	assert.Equal(t, 10, getTokenFromDB(t, child.Id).DailyUsedCount)
}
