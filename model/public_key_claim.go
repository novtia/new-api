package model

import (
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var (
	ErrPublicKeyAlreadyClaimed = errors.New("this Discord account has already claimed a public key")
	ErrNoPublicPool            = errors.New("no public pool is available right now")
)

// PublicKeyClaim 记录“哪个 Discord 账号领取了哪个访客 Key”。
// DiscordId 唯一索引是“一人一 Key”的最终保证，不依赖应用层的先查后写。
type PublicKeyClaim struct {
	Id          int    `json:"id" gorm:"primaryKey"`
	DiscordId   string `json:"discord_id" gorm:"type:varchar(32);uniqueIndex"`
	DiscordName string `json:"discord_name" gorm:"type:varchar(128)"`
	TokenId     int    `json:"token_id" gorm:"index"`
	PoolId      int    `json:"pool_id" gorm:"index"`
	ClientIp    string `json:"client_ip" gorm:"type:varchar(64);index"`
	CreatedAt   int64  `json:"created_at" gorm:"bigint;index"`
	Banned      bool   `json:"banned" gorm:"default:false"`
}

func GetPublicKeyClaimByDiscordID(discordID string) (*PublicKeyClaim, error) {
	var c PublicKeyClaim
	if err := DB.Where("discord_id = ?", discordID).Take(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func CountPublicKeyClaimsByIPSince(ip string, since int64) (int64, error) {
	var n int64
	err := DB.Model(&PublicKeyClaim{}).Where("client_ip = ? AND created_at >= ?", ip, since).Count(&n).Error
	return n, err
}

func CountPublicKeyClaimsSince(since int64) (int64, error) {
	var n int64
	err := DB.Model(&PublicKeyClaim{}).Where("created_at >= ?", since).Count(&n).Error
	return n, err
}

// PickPublicPool 选出当前可用且剩余额度最多的公益密钥。
func PickPublicPool() (*Token, error) {
	var pools []Token
	if err := DB.Where("is_public_pool = ? AND status = ?", true, common.TokenStatusEnabled).
		Order("remain_quota desc").Find(&pools).Error; err != nil {
		return nil, err
	}
	for i := range pools {
		if poolParentUsable(&pools[i]) {
			return &pools[i], nil
		}
	}
	return nil, ErrNoPublicPool
}

// CreatePublicKeyClaim 在同一事务内创建访客 Key 与领取记录。
// 同一个 Discord 账号并发领取时，唯一索引保证只有一个事务成功，其余整体回滚。
func CreatePublicKeyClaim(pool *Token, claim *PublicKeyClaim, key string) (*Token, error) {
	now := common.GetTimestamp()
	dailyCap, hourlyCap := EffectivePoolCaps(pool)
	dailyCount, dailyImages := EffectivePoolCountCaps(pool)
	child := &Token{
		UserId:             pool.UserId,
		Name:               "公益-" + claim.DiscordName,
		Key:                key,
		Status:             common.TokenStatusEnabled,
		CreatedTime:        now,
		AccessedTime:       now,
		ExpiredTime:        -1,
		UnlimitedQuota:     true,
		ModelLimitsEnabled: pool.ModelLimitsEnabled,
		ModelLimits:        pool.ModelLimits,
		Group:              pool.Group,
		CrossGroupRetry:    pool.CrossGroupRetry,
		AutoGroups:         pool.AutoGroups,
		// 访客 Key 不允许查询账户余额，余额页只展示限额信息。
		AllowUserQuotaQuery: false,
		PoolParentId:        pool.Id,
		DailyCapQuota:       dailyCap,
		HourlyCapQuota:      hourlyCap,
		DailyCapCount:       dailyCount,
		DailyCapImages:      dailyImages,
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(child).Error; err != nil {
			return err
		}
		claim.TokenId = child.Id
		claim.PoolId = pool.Id
		claim.CreatedAt = now
		return tx.Create(claim).Error
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrPublicKeyAlreadyClaimed
		}
		return nil, err
	}
	return child, nil
}

func isUniqueViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}

// ListPublicKeyClaims 供后台查看领取记录，附带每个访客 Key 的当日用量。
type PublicKeyClaimView struct {
	PublicKeyClaim
	DailyUsedQuota  int   `json:"daily_used_quota"`
	DailyCapQuota   int   `json:"daily_cap_quota"`
	UsedQuota       int   `json:"used_quota"`
	DailyUsedCount  int   `json:"daily_used_count"`
	DailyUsedImages int   `json:"daily_used_images"`
	PoolFlaggedAt   int64 `json:"pool_flagged_at"`
	TokenStatus     int   `json:"token_status"`
}

func ListPublicKeyClaims(offset, limit int) ([]PublicKeyClaimView, int64, error) {
	var total int64
	if err := DB.Model(&PublicKeyClaim{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var claims []PublicKeyClaim
	if err := DB.Order("id desc").Offset(offset).Limit(limit).Find(&claims).Error; err != nil {
		return nil, 0, err
	}
	views := make([]PublicKeyClaimView, 0, len(claims))
	for _, c := range claims {
		v := PublicKeyClaimView{PublicKeyClaim: c}
		var t Token
		if err := DB.Select("id", "status", "used_quota", "daily_cap_quota", "daily_used_quota", "daily_reset_day", "pool_flagged_at",
			"daily_used_count", "daily_used_images").
			Where("id = ?", c.TokenId).Take(&t).Error; err == nil {
			info := GetPoolChildInfo(&Token{PoolParentId: 1, DailyCapQuota: t.DailyCapQuota,
				DailyUsedQuota: t.DailyUsedQuota, DailyResetDay: t.DailyResetDay,
				DailyUsedCount: t.DailyUsedCount, DailyUsedImages: t.DailyUsedImages}, time.Now())
			v.DailyUsedQuota = info.DailyUsedQuota
			v.DailyUsedCount = info.DailyUsedCount
			v.DailyUsedImages = info.DailyUsedImages
			v.DailyCapQuota = t.DailyCapQuota
			v.UsedQuota = t.UsedQuota
			v.PoolFlaggedAt = t.PoolFlaggedAt
			v.TokenStatus = t.Status
		}
		views = append(views, v)
	}
	return views, total, nil
}

// BanPublicKeyClaim 封禁某个领取记录：标记 banned 并停用其访客 Key。
// 因为 DiscordId 唯一，被封禁的账号无法重新领取。
func BanPublicKeyClaim(claimID int) error {
	var c PublicKeyClaim
	if err := DB.Where("id = ?", claimID).Take(&c).Error; err != nil {
		return err
	}
	if err := DB.Model(&PublicKeyClaim{}).Where("id = ?", claimID).Update("banned", true).Error; err != nil {
		return err
	}
	token, err := GetTokenById(c.TokenId)
	if err != nil {
		return err
	}
	token.Status = common.TokenStatusDisabled
	return token.SelectUpdate()
}
