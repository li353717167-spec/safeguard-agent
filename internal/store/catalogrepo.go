package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ConfigChange 对应 config_changes 表：配置变更台账。
type ConfigChange struct {
	IdemKey   string    `gorm:"column:idem_key;primaryKey;size:32"`
	Service   string    `gorm:"column:service;size:64;not null"`
	ConfigKey string    `gorm:"column:config_key;size:64;not null"`
	OldValue  *string   `gorm:"column:old_value;size:255"`
	NewValue  string    `gorm:"column:new_value;size:255;not null"`
	AppliedAt time.Time `gorm:"column:applied_at;not null"`
}

func (ConfigChange) TableName() string { return "config_changes" }

// CatalogRepo 是服务目录的**写**操作。
//
// 它是写工具真正的"下游"：写工具不自己拼 SQL，而是调这里。
// 这样"下游幂等"只有一个实现点，换存储时工具代码不用动。
type CatalogRepo struct {
	db *gorm.DB
}

// NewCatalogRepo 构造仓储。
func NewCatalogRepo(db *gorm.DB) *CatalogRepo { return &CatalogRepo{db: db} }

// ConfigValue 读一条配置当前值；不存在时返回 ("", false, nil)。
func (r *CatalogRepo) ConfigValue(ctx context.Context, service, key string) (string, bool, error) {
	var row ServiceConfig
	err := r.db.WithContext(ctx).
		Where("service = ? AND config_key = ?", service, key).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: 读取配置 %s.%s 失败: %w", service, key, err)
	}
	return row.ConfigValue, true, nil
}

// ApplyConfig 应用一次配置变更，并保证**同一个幂等 key 只生效一次**。
//
// 返回值 applied=false 表示"这个 key 已经生效过"，本次没有重复修改——
// 这就是下游幂等：即使本地状态表丢了，下游也不会被重复改。
//
// 实现要点：先尝试把 key 写进 config_changes（主键唯一），成功才去改 service_configs。
// 顺序不能反：先改配置再记 key，中间崩溃就会变成"改了但没记账"，下次又会改一遍。
func (r *CatalogRepo) ApplyConfig(ctx context.Context, idemKey, service, key, newValue string) (oldValue string, applied bool, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current ServiceConfig
		readErr := tx.Where("service = ? AND config_key = ?", service, key).First(&current).Error
		if readErr != nil && !errors.Is(readErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("读取当前配置失败: %w", readErr)
		}
		if readErr == nil {
			oldValue = current.ConfigValue
		}

		// ① 先用幂等 key 占位：主键冲突 = 这个变更已经生效过。
		ledger := &ConfigChange{
			IdemKey:   idemKey,
			Service:   service,
			ConfigKey: key,
			NewValue:  newValue,
			AppliedAt: time.Now().UTC(),
		}
		if readErr == nil {
			prev := oldValue
			ledger.OldValue = &prev
		}
		if createErr := tx.Create(ledger).Error; createErr != nil {
			if isDuplicateEntry(createErr) {
				applied = false
				return nil // 已生效：不重复修改配置
			}
			return fmt.Errorf("写入变更台账失败: %w", createErr)
		}

		// ② 再改配置本体。
		if readErr == nil {
			res := tx.Model(&ServiceConfig{}).
				Where("service = ? AND config_key = ?", service, key).
				Update("config_value", newValue)
			if res.Error != nil {
				return fmt.Errorf("更新配置失败: %w", res.Error)
			}
			return nil
		}

		row := &ServiceConfig{
			Service:     service,
			ConfigKey:   key,
			ConfigValue: newValue,
			UpdatedAt:   time.Now().UTC(),
		}
		if createErr := tx.Create(row).Error; createErr != nil {
			return fmt.Errorf("新增配置失败: %w", createErr)
		}
		return nil
	})
	if err != nil {
		return "", false, fmt.Errorf("store: 应用配置变更失败: %w", err)
	}
	return oldValue, applied, nil
}

// SetServiceStatus 改服务健康状态（restart_service 的效果就落在这里）。
func (r *CatalogRepo) SetServiceStatus(ctx context.Context, service, status string) error {
	res := r.db.WithContext(ctx).
		Model(&Service{}).
		Where("name = ?", service).
		Select("status", "updated_at").
		Updates(Service{Status: status, UpdatedAt: time.Now().UTC()})
	if res.Error != nil {
		return fmt.Errorf("store: 更新服务状态失败: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("store: 服务 %s 不存在", service)
	}
	return nil
}

// CountConfigChanges 统计某个服务被改过多少次（评测/演示用）。
func (r *CatalogRepo) CountConfigChanges(ctx context.Context, service string) (int64, error) {
	var n int64
	if err := r.db.WithContext(ctx).Model(&ConfigChange{}).Where("service = ?", service).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("store: 统计变更台账失败: %w", err)
	}
	return n, nil
}
