package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/setting"
)

// MigrateSSOSettings fills absent settings once; false and empty rows win.
func (r *settingRepository) MigrateSSOSettings(ctx context.Context, fallback map[string]string) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	marker, err := tx.Setting.Query().Where(setting.KeyEQ("migration_sso_auth_v1")).ForUpdate().Only(ctx)
	if err != nil {
		return fmt.Errorf("read SSO migration marker: %w", err)
	}
	rows, err := tx.Setting.Query().Where(setting.KeyIn("sso_issuer_url", "sso_enabled", "sso_only_enabled", "sso_registration_enabled", "sso_organization", "sso_application")).All(ctx)
	if err != nil {
		return err
	}
	present := make(map[string]bool)
	for _, row := range rows {
		present[row.Key] = true
	}
	if marker.Value == "done" {
		if !present["sso_issuer_url"] {
			return fmt.Errorf("SSO configuration missing after migration")
		}
		return tx.Commit()
	}
	if marker.Value != "pending" {
		return fmt.Errorf("invalid SSO migration state")
	}
	for key, value := range fallback {
		if present[key] {
			continue
		}
		if err := tx.Setting.Create().SetKey(key).SetValue(value).SetUpdatedAt(time.Now()).OnConflictColumns(setting.FieldKey).DoNothing().Exec(ctx); err != nil {
			return err
		}
	}
	if _, err := tx.Setting.UpdateOne(marker).SetValue("done").Save(ctx); err != nil {
		return err
	}
	return tx.Commit()
}
