package model

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// 公益池（public pool）
//
// 管理员把某个令牌标记为公益密钥（Token.IsPublicPool）后，访客可以领取各自的子 Key
// （Token.PoolParentId 指向该公益密钥）。子 Key 本身不限额（UnlimitedQuota），
// 真正的约束来自三处：
//  1. 每个子 Key 自己的每日上限和每小时上限（滑动计数器，惰性重置）；
//  2. 子 Key 的每一笔预扣、结算、退款都会同步作用到公益密钥上，
//     所以子 Key 的总消耗永远不会超过公益密钥的剩余额度；
//  3. 每个子 Key 同时只能有一个进行中的调用，请求返回后才允许下一次。
//
// 所有额度变动都经过 TryReserveTokenQuota / DecreaseTokenQuota / IncreaseTokenQuota，
// 公益池逻辑只挂在这三个入口上，计费主流程无需感知。

var (
	ErrPublicPoolExhausted  = errors.New("public key pool is exhausted")
	ErrPublicPoolDailyLimit = errors.New("daily quota limit reached for this public key, resets at 00:00 (UTC+8)")
	ErrPublicPoolHourlyLim  = errors.New("hourly quota limit reached for this public key, please slow down")

	ErrPublicPoolDailyCountLimit = errors.New("daily request limit reached for this public key, resets at 00:00 (UTC+8)")
	ErrPublicPoolDailyImageLimit = errors.New("daily image limit reached for this public key, resets at 00:00 (UTC+8)")
)

// 每日额度以东八区自然日为界。使用固定偏移而不是 LoadLocation，避免精简系统缺少 tzdata。
var poolLocation = time.FixedZone("Asia/Shanghai", 8*3600)

// PoolDayKey 返回形如 20261014 的东八区日期编号。
func PoolDayKey(t time.Time) int {
	y, m, d := t.In(poolLocation).Date()
	return y*10000 + int(m)*100 + d
}

func poolHourKey(t time.Time) int64 {
	return t.Unix() / 3600
}

// PoolNextResetAt 返回下一次东八区零点的 unix 时间戳。
func PoolNextResetAt(t time.Time) int64 {
	local := t.In(poolLocation)
	next := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, poolLocation)
	return next.Unix()
}

// PublicPoolDailyCapQuota 访客 Key 每日上限（额度单位）。可用环境变量 PUBLIC_POOL_DAILY_CAP_USD 调整，默认 10 美元。
func PublicPoolDailyCapQuota() int {
	usd := common.GetEnvOrDefault("PUBLIC_POOL_DAILY_CAP_USD", 10)
	return int(float64(usd) * common.QuotaPerUnit)
}

// PublicPoolHourlyCapQuota 访客 Key 每小时上限，默认为每日上限的 40%。
func PublicPoolHourlyCapQuota() int {
	return PublicPoolDailyCapQuota() * poolHourlyPercent() / 100
}

func poolHourlyPercent() int {
	return common.GetEnvOrDefault("PUBLIC_POOL_HOURLY_CAP_PERCENT", 40)
}

// EffectivePoolCaps 返回某个公益密钥下每位访客的每日、每小时上限。
// 上限由管理员在公益密钥上设置（存放在其 DailyCapQuota / HourlyCapQuota 字段）；
// 留空（0）时每日使用默认值，每小时使用每日上限的默认比例。
func EffectivePoolCaps(pool *Token) (daily, hourly int) {
	daily = pool.DailyCapQuota
	if daily <= 0 {
		daily = PublicPoolDailyCapQuota()
	}
	hourly = pool.HourlyCapQuota
	if hourly <= 0 {
		hourly = daily * poolHourlyPercent() / 100
	}
	return daily, hourly
}

// EffectivePoolCountCaps 返回某个公益密钥下每位访客的每日请求次数和每日图片张数上限，0 表示不限制。
// 公益密钥自身的字段为 0 时回落到环境变量 PUBLIC_POOL_DAILY_CAP_COUNT / PUBLIC_POOL_DAILY_CAP_IMAGES，
// 二者默认都是 0，所以不配置时不改变现有公益 Key 的行为。
func EffectivePoolCountCaps(pool *Token) (requests, images int) {
	requests = pool.DailyCapCount
	if requests <= 0 {
		requests = max(common.GetEnvOrDefault("PUBLIC_POOL_DAILY_CAP_COUNT", 0), 0)
	}
	images = pool.DailyCapImages
	if images <= 0 {
		images = max(common.GetEnvOrDefault("PUBLIC_POOL_DAILY_CAP_IMAGES", 0), 0)
	}
	return requests, images
}

// PropagatePoolLimits 把公益密钥当前的访客限额同步给它名下已领取的所有访客 Key，
// 管理员在后台调整限额后立即对已有访客生效。
func PropagatePoolLimits(pool *Token) error {
	daily, hourly := EffectivePoolCaps(pool)
	requests, images := EffectivePoolCountCaps(pool)
	return DB.Model(&Token{}).Where("pool_parent_id = ?", pool.Id).
		Updates(map[string]any{
			"daily_cap_quota":  daily,
			"hourly_cap_quota": hourly,
			"daily_cap_count":  requests,
			"daily_cap_images": images,
		}).Error
}

type poolLink struct {
	parentID int
}

// poolLinks 缓存 tokenID -> 公益池关系。nil 值表示该令牌不是访客 Key。
// 父子关系在令牌创建后不会改变，删除令牌时清除缓存。
var poolLinks sync.Map

func forgetPoolLink(tokenID int) {
	poolLinks.Delete(tokenID)
}

// ResetPoolLinkCache 清空缓存，仅供测试使用。
func ResetPoolLinkCache() {
	poolLinks.Range(func(k, _ any) bool {
		poolLinks.Delete(k)
		return true
	})
}

func getPoolLink(tokenID int) (*poolLink, error) {
	if v, ok := poolLinks.Load(tokenID); ok {
		return v.(*poolLink), nil
	}
	var t Token
	if err := DB.Select("id", "pool_parent_id").Where("id = ?", tokenID).Take(&t).Error; err != nil {
		return nil, err
	}
	var link *poolLink
	if t.PoolParentId != 0 {
		link = &poolLink{parentID: t.PoolParentId}
	}
	poolLinks.Store(tokenID, link)
	return link, nil
}

func loadPoolParent(parentID int) (*Token, error) {
	var p Token
	if err := DB.Where("id = ?", parentID).Take(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func poolParentUsable(p *Token) bool {
	if !p.IsPublicPool || p.Status != common.TokenStatusEnabled {
		return false
	}
	if p.ExpiredTime != -1 && p.ExpiredTime < common.GetTimestamp() {
		return false
	}
	if !p.UnlimitedQuota && p.RemainQuota <= 0 {
		return false
	}
	return true
}

// IsPublicPoolChild 判断令牌是否为访客公益 Key。计费会话据此禁用“信任旁路”，
// 保证访客 Key 的每次请求都会走预扣，从而强制执行每日/每小时上限。
func IsPublicPoolChild(tokenID int) bool {
	link, err := getPoolLink(tokenID)
	return err == nil && link != nil
}

// publicPoolInflight 记录正在处理的访客 Key。进程内生效；请求正常返回、失败或中断时释放。
var publicPoolInflight sync.Map

// AcquirePublicPoolCall 为访客公益 Key 占用唯一的进行中名额。
// 普通令牌不受限制，返回的 release 是空操作。名额已被占用时 ok 为 false，release 为 nil。
func AcquirePublicPoolCall(tokenID int) (release func(), ok bool) {
	if !IsPublicPoolChild(tokenID) {
		return func() {}, true
	}
	if _, loaded := publicPoolInflight.LoadOrStore(tokenID, struct{}{}); loaded {
		return nil, false
	}
	return func() { publicPoolInflight.Delete(tokenID) }, true
}

// validatePoolChild 在令牌鉴权阶段检查公益池是否仍可用。非访客 Key 直接放行。
func validatePoolChild(tokenID int) error {
	link, err := getPoolLink(tokenID)
	if err != nil || link == nil {
		return nil
	}
	parent, err := loadPoolParent(link.parentID)
	if err != nil || !poolParentUsable(parent) {
		return ErrPublicPoolExhausted
	}
	// 窗口额度已用尽时在鉴权阶段直接拒绝，覆盖不经过统一预扣的计费路径（如异步任务）。
	var t Token
	if err := DB.Select("id", "daily_cap_quota", "daily_used_quota", "daily_reset_day",
		"hourly_cap_quota", "hourly_used_quota", "hourly_window",
		"daily_cap_count", "daily_used_count", "daily_cap_images", "daily_used_images").
		Where("id = ?", tokenID).Take(&t).Error; err != nil {
		return nil
	}
	info := GetPoolChildInfo(&Token{
		PoolParentId: link.parentID, DailyCapQuota: t.DailyCapQuota, DailyUsedQuota: t.DailyUsedQuota,
		DailyResetDay: t.DailyResetDay, HourlyCapQuota: t.HourlyCapQuota, HourlyUsedQuota: t.HourlyUsedQuota,
		HourlyWindow: t.HourlyWindow, DailyCapCount: t.DailyCapCount, DailyUsedCount: t.DailyUsedCount,
		DailyCapImages: t.DailyCapImages, DailyUsedImages: t.DailyUsedImages,
	}, time.Now())
	if info.DailyCapQuota > 0 && info.DailyUsedQuota >= info.DailyCapQuota {
		return ErrPublicPoolDailyLimit
	}
	// 图片张数用尽只在预留阶段拒绝：同一个 Key 仍可继续发文本请求，这里只拦请求次数。
	if info.DailyCapCount > 0 && info.DailyUsedCount >= info.DailyCapCount {
		return ErrPublicPoolDailyCountLimit
	}
	if info.HourlyCapQuota > 0 && info.HourlyUsedQuota >= info.HourlyCapQuota {
		return ErrPublicPoolHourlyLim
	}
	return nil
}

// rollPoolWindows 若跨日或跨小时，则把对应计数器清零。语句幂等，并发安全。
func rollPoolWindows(id int, now time.Time) error {
	day := PoolDayKey(now)
	hour := poolHourKey(now)
	if err := DB.Model(&Token{}).
		Where("id = ? AND daily_reset_day <> ?", id, day).
		Updates(map[string]any{
			"daily_used_quota":  0,
			"daily_used_count":  0,
			"daily_used_images": 0,
			"daily_reset_day":   day,
		}).Error; err != nil {
		return err
	}
	return DB.Model(&Token{}).
		Where("id = ? AND hourly_window <> ?", id, hour).
		Updates(map[string]any{"hourly_used_quota": 0, "hourly_window": hour}).Error
}

// reservePoolChildWindow 原子地占用子 Key 的每日、每小时额度，任一上限超出则不占用。
func reservePoolChildWindow(id, quota int, now time.Time) error {
	if err := rollPoolWindows(id, now); err != nil {
		return err
	}
	res := DB.Model(&Token{}).
		Where("id = ? AND daily_cap_quota > 0 AND daily_used_quota + ? <= daily_cap_quota"+
			" AND (hourly_cap_quota <= 0 OR hourly_used_quota + ? <= hourly_cap_quota)", id, quota, quota).
		Updates(map[string]any{
			"daily_used_quota":  gorm.Expr("daily_used_quota + ?", quota),
			"hourly_used_quota": gorm.Expr("hourly_used_quota + ?", quota),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 1 {
		return nil
	}

	var t Token
	if err := DB.Select("id", "daily_cap_quota", "daily_used_quota", "hourly_cap_quota", "hourly_used_quota").
		Where("id = ?", id).Take(&t).Error; err != nil {
		return err
	}
	if t.DailyCapQuota <= 0 || t.DailyUsedQuota+quota > t.DailyCapQuota {
		return ErrPublicPoolDailyLimit
	}
	// 每小时上限被触发：短时间内烧掉大量额度，记录标记供后台审查。
	// 若本小时尚未消耗任何额度，说明只是单次预估过大，并非刷额度，不标记。
	if t.HourlyUsedQuota > 0 {
		flagPoolChild(id, now)
	}
	return ErrPublicPoolHourlyLim
}

func flagPoolChild(id int, now time.Time) {
	res := DB.Model(&Token{}).Where("id = ? AND pool_flagged_at = 0", id).Update("pool_flagged_at", now.Unix())
	if res.Error == nil && res.RowsAffected == 1 {
		common.SysLog(fmt.Sprintf("public pool: token %d hit hourly velocity cap, flagged", id))
	}
}

// refundPoolChildWindow 归还子 Key 窗口内已占用的额度，计数器不会低于 0。
func refundPoolChildWindow(id, quota int) {
	if quota <= 0 {
		return
	}
	err := DB.Model(&Token{}).Where("id = ?", id).Updates(map[string]any{
		"daily_used_quota":  gorm.Expr("CASE WHEN daily_used_quota > ? THEN daily_used_quota - ? ELSE 0 END", quota, quota),
		"hourly_used_quota": gorm.Expr("CASE WHEN hourly_used_quota > ? THEN hourly_used_quota - ? ELSE 0 END", quota, quota),
	}).Error
	if err != nil {
		common.SysError(fmt.Sprintf("public pool: failed to refund window for token %d: %v", id, err))
	}
}

// ReservePoolChildUsage 原子占用访客 Key 当日的请求次数和图片张数，任一上限超出则都不占用。
// 它独立于额度预扣：免费模型不会预扣额度，次数和张数仍必须被限制。
// reserved=false 且 err=nil 表示该令牌不是访客 Key，无需退还。
func ReservePoolChildUsage(tokenID, requests, images int) (reserved bool, err error) {
	if requests < 0 || images < 0 {
		return false, errors.New("pool usage cannot be negative")
	}
	link, err := getPoolLink(tokenID)
	if err != nil {
		return false, err
	}
	if link == nil {
		return false, nil
	}
	now := time.Now()
	if err := rollPoolWindows(tokenID, now); err != nil {
		return false, err
	}
	res := DB.Model(&Token{}).
		Where("id = ? AND (daily_cap_count <= 0 OR daily_used_count + ? <= daily_cap_count)"+
			" AND (daily_cap_images <= 0 OR daily_used_images + ? <= daily_cap_images)", tokenID, requests, images).
		Updates(map[string]any{
			"daily_used_count":  gorm.Expr("daily_used_count + ?", requests),
			"daily_used_images": gorm.Expr("daily_used_images + ?", images),
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 1 {
		return true, nil
	}

	var t Token
	if err := DB.Select("id", "daily_cap_count", "daily_used_count", "daily_cap_images", "daily_used_images").
		Where("id = ?", tokenID).Take(&t).Error; err != nil {
		return false, err
	}
	if t.DailyCapCount > 0 && t.DailyUsedCount+requests > t.DailyCapCount {
		return false, ErrPublicPoolDailyCountLimit
	}
	return false, ErrPublicPoolDailyImageLimit
}

// RefundPoolChildUsage 归还 ReservePoolChildUsage 占用的次数和张数，计数不会低于 0。
func RefundPoolChildUsage(tokenID, requests, images int) {
	if requests <= 0 && images <= 0 {
		return
	}
	err := DB.Model(&Token{}).Where("id = ?", tokenID).Updates(map[string]any{
		"daily_used_count":  gorm.Expr("CASE WHEN daily_used_count > ? THEN daily_used_count - ? ELSE 0 END", requests, requests),
		"daily_used_images": gorm.Expr("CASE WHEN daily_used_images > ? THEN daily_used_images - ? ELSE 0 END", images, images),
	}).Error
	if err != nil {
		common.SysError(fmt.Sprintf("public pool: failed to refund usage for token %d: %v", tokenID, err))
	}
}

// tryReservePoolChild 处理访客 Key 的预扣。handled=false 表示该令牌不是访客 Key，交回原流程。
func tryReservePoolChild(id int, key string, quota int) (reserved, handled bool, err error) {
	link, lerr := getPoolLink(id)
	if lerr != nil || link == nil {
		return false, false, nil
	}

	parent, perr := loadPoolParent(link.parentID)
	if perr != nil || !poolParentUsable(parent) {
		return false, true, ErrPublicPoolExhausted
	}
	if err := reservePoolChildWindow(id, quota, time.Now()); err != nil {
		return false, true, err
	}
	// 先从公益密钥扣：池子不够则整体失败，并归还刚占用的窗口额度。
	ok, err := TryReserveTokenQuota(parent.Id, parent.Key, quota, parent.UnlimitedQuota)
	if err != nil || !ok {
		refundPoolChildWindow(id, quota)
		if err != nil {
			return false, true, err
		}
		return false, true, ErrPublicPoolExhausted
	}
	if err := decreaseTokenQuotaCore(id, key, quota); err != nil {
		refundPoolChild(link, id, quota)
		return false, true, err
	}
	return true, true, nil
}

// refundPoolChild 归还子 Key 窗口额度，并把额度退回公益密钥。
func refundPoolChild(link *poolLink, id, quota int) {
	refundPoolChildWindow(id, quota)
	parent, err := loadPoolParent(link.parentID)
	if err != nil {
		common.SysError(fmt.Sprintf("public pool: parent %d missing while refunding token %d: %v", link.parentID, id, err))
		return
	}
	if err := IncreaseTokenQuota(parent.Id, parent.Key, quota); err != nil {
		common.SysError(fmt.Sprintf("public pool: failed to refund parent %d: %v", parent.Id, err))
	}
}

// poolChildRefund 是 IncreaseTokenQuota 的钩子：退款路径。
func poolChildRefund(id, quota int) {
	if quota <= 0 {
		return
	}
	link, err := getPoolLink(id)
	if err != nil || link == nil {
		return
	}
	refundPoolChild(link, id, quota)
}

// poolChildConsume 是 DecreaseTokenQuota 的钩子：结算阶段补扣（实际用量大于预扣）。
func poolChildConsume(id, quota int) {
	if quota <= 0 {
		return
	}
	link, err := getPoolLink(id)
	if err != nil || link == nil {
		return
	}
	now := time.Now()
	if err := rollPoolWindows(id, now); err != nil {
		common.SysError(fmt.Sprintf("public pool: failed to roll windows for token %d: %v", id, err))
	}
	err = DB.Model(&Token{}).Where("id = ?", id).Updates(map[string]any{
		"daily_used_quota":  gorm.Expr("daily_used_quota + ?", quota),
		"hourly_used_quota": gorm.Expr("hourly_used_quota + ?", quota),
	}).Error
	if err != nil {
		common.SysError(fmt.Sprintf("public pool: failed to add window usage for token %d: %v", id, err))
	}
	parent, err := loadPoolParent(link.parentID)
	if err != nil {
		common.SysError(fmt.Sprintf("public pool: parent %d missing while charging token %d: %v", link.parentID, id, err))
		return
	}
	if err := DecreaseTokenQuota(parent.Id, parent.Key, quota); err != nil {
		common.SysError(fmt.Sprintf("public pool: failed to charge parent %d: %v", parent.Id, err))
	}
}

// PoolChildInfo 是访客 Key 对外展示的限额信息（不含公益密钥的余额）。
type PoolChildInfo struct {
	IsPoolChild     bool  `json:"is_pool_child"`
	DailyCapQuota   int   `json:"daily_cap_quota"`
	DailyUsedQuota  int   `json:"daily_used_quota"`
	HourlyCapQuota  int   `json:"hourly_cap_quota"`
	HourlyUsedQuota int   `json:"hourly_used_quota"`
	ResetAt         int64 `json:"reset_at"`
	PoolAvailable   bool  `json:"pool_available"`
	// 每日请求次数与图片张数，上限为 0 表示不限制。
	DailyCapCount   int `json:"daily_cap_count"`
	DailyUsedCount  int `json:"daily_used_count"`
	DailyCapImages  int `json:"daily_cap_images"`
	DailyUsedImages int `json:"daily_used_images"`
}

// GetPoolChildInfoByID 按令牌 ID 从数据库读取限额视图（不依赖可能缺少公益字段的缓存对象）。
func GetPoolChildInfoByID(tokenID int, now time.Time) (PoolChildInfo, bool) {
	t, err := GetTokenById(tokenID)
	if err != nil || t.PoolParentId == 0 {
		return PoolChildInfo{}, false
	}
	return GetPoolChildInfo(t, now), true
}

// GetPoolChildInfo 返回访客 Key 的限额视图，已按当前时间应用惰性重置（只读，不写库）。
func GetPoolChildInfo(token *Token, now time.Time) PoolChildInfo {
	if token == nil || token.PoolParentId == 0 {
		return PoolChildInfo{}
	}
	info := PoolChildInfo{
		IsPoolChild:     true,
		DailyCapQuota:   token.DailyCapQuota,
		DailyUsedQuota:  token.DailyUsedQuota,
		HourlyCapQuota:  token.HourlyCapQuota,
		HourlyUsedQuota: token.HourlyUsedQuota,
		ResetAt:         PoolNextResetAt(now),
		DailyCapCount:   token.DailyCapCount,
		DailyUsedCount:  token.DailyUsedCount,
		DailyCapImages:  token.DailyCapImages,
		DailyUsedImages: token.DailyUsedImages,
	}
	if token.DailyResetDay != PoolDayKey(now) {
		info.DailyUsedQuota = 0
		info.DailyUsedCount = 0
		info.DailyUsedImages = 0
	}
	if token.HourlyWindow != poolHourKey(now) {
		info.HourlyUsedQuota = 0
	}
	if parent, err := loadPoolParent(token.PoolParentId); err == nil {
		info.PoolAvailable = poolParentUsable(parent)
	}
	return info
}
