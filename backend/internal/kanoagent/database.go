package kanoagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Default  string `json:"default"`
}

type TableSnapshot struct {
	Columns []Column `json:"columns"`
	Count   int64    `json:"count"`
	Digest  string   `json:"digest"`
}

type DatabaseSnapshot struct {
	Tables     map[string]TableSnapshot `json:"tables"`
	Migrations map[string]string        `json:"migrations"`
	Settings   map[string]string        `json:"settings"`
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
func quoteLiteral(value string) string    { return `'` + strings.ReplaceAll(value, `'`, `''`) + `'` }

func (e *Engine) pgArgs(container Container, query string) []string {
	environment := container.Environment()
	return []string{"docker", "exec", "-e", "PGOPTIONS=-c timezone=UTC", container.ID, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", environment["POSTGRES_USER"], "-d", environment["POSTGRES_DB"], "-At", "-c", query}
}

func (e *Engine) pg(ctx context.Context, container Container, query string) (string, error) {
	environment := container.Environment()
	if environment["POSTGRES_USER"] == "" || environment["POSTGRES_DB"] == "" {
		return "", errors.New("postgresql runtime identity is missing")
	}
	value, err := e.run(ctx, e.pgArgs(container, query)...)
	return strings.TrimSpace(string(value)), err
}

func (e *Engine) database(ctx context.Context, container Container, projection *DatabaseSnapshot) (DatabaseSnapshot, error) {
	result := DatabaseSnapshot{Tables: map[string]TableSnapshot{}, Migrations: map[string]string{}, Settings: map[string]string{}}
	value, err := e.pg(ctx, container, "SELECT COALESCE(jsonb_agg(tablename ORDER BY tablename),'[]'::jsonb) FROM pg_tables WHERE schemaname='public'")
	if err != nil {
		return result, err
	}
	var tables []string
	if json.Unmarshal([]byte(value), &tables) != nil {
		return result, errors.New("invalid PostgreSQL table catalog")
	}
	for _, table := range tables {
		value, err = e.pg(ctx, container, "SELECT COALESCE(jsonb_agg(jsonb_build_object('name',column_name,'type',udt_name,'nullable',is_nullable='YES','default',COALESCE(column_default,'')) ORDER BY ordinal_position),'[]'::jsonb) FROM information_schema.columns WHERE table_schema='public' AND table_name="+quoteLiteral(table))
		if err != nil {
			return result, err
		}
		var columns []Column
		if json.Unmarshal([]byte(value), &columns) != nil || len(columns) == 0 {
			return result, errors.New("invalid PostgreSQL column catalog")
		}
		selected := columns
		if projection != nil {
			if original, ok := projection.Tables[table]; ok {
				selected = original.Columns
			}
		}
		selection := make([]string, 0, len(selected))
		for _, column := range selected {
			selection = append(selection, quoteIdentifier(column.Name))
		}
		query := "SELECT COUNT(*),COALESCE(SUM(('x'||substr(h,1,16))::bit(64)::bigint::numeric),0),COALESCE(SUM(('x'||substr(h,17,16))::bit(64)::bigint::numeric),0) FROM (SELECT md5(row_to_json(t)::jsonb::text) AS h FROM (SELECT " + strings.Join(selection, ",") + " FROM public." + quoteIdentifier(table) + " OFFSET 0) t) rows"
		value, err = e.pg(ctx, container, query)
		if err != nil {
			return result, err
		}
		parts := strings.Split(value, "|")
		if len(parts) != 3 {
			return result, errors.New("invalid PostgreSQL fingerprint")
		}
		count, parseErr := strconv.ParseInt(parts[0], 10, 64)
		if parseErr != nil {
			return result, errors.New("invalid PostgreSQL row count")
		}
		digest := sha256.Sum256([]byte(value))
		result.Tables[table] = TableSnapshot{Columns: columns, Count: count, Digest: hex.EncodeToString(digest[:])}
	}
	value, err = e.pg(ctx, container, "SELECT COALESCE(jsonb_object_agg(filename,checksum),'{}'::jsonb) FROM schema_migrations")
	if err != nil {
		return result, err
	}
	if json.Unmarshal([]byte(value), &result.Migrations) != nil {
		return result, errors.New("invalid migration checksums")
	}
	value, err = e.pg(ctx, container, "SELECT COALESCE(jsonb_object_agg(key,value),'{}'::jsonb) FROM settings")
	if err != nil {
		return result, err
	}
	if json.Unmarshal([]byte(value), &result.Settings) != nil {
		return result, errors.New("invalid settings snapshot")
	}
	return result, nil
}

func pausedSettings(original map[string]string) (map[string]string, error) {
	result := map[string]string{}
	for key, value := range original {
		result[key] = value
	}
	result["channel_monitor_enabled"] = "false"
	for _, entry := range []struct {
		key, field string
		value      any
		defaults   map[string]any
	}{
		{"ops_runtime_log_config", "request_retention_days", 0, map[string]any{}},
		{"upstream_billing_probe_settings", "enabled", false, map[string]any{"interval_minutes": 10}},
	} {
		value := entry.defaults
		if stored, ok := original[entry.key]; ok {
			if json.Unmarshal([]byte(stored), &value) != nil {
				return nil, errors.New("invalid runtime background settings")
			}
		}
		if value == nil {
			value = map[string]any{}
		}
		value[entry.field] = entry.value
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		result[entry.key] = string(body)
	}
	return result, nil
}

func (e *Engine) pausePrivateSettings(ctx context.Context, container Container, original map[string]string) error {
	paused, err := pausedSettings(original)
	if err != nil {
		return err
	}
	for _, key := range []string{"channel_monitor_enabled", "ops_runtime_log_config", "upstream_billing_probe_settings"} {
		_, err = e.pg(ctx, container, "INSERT INTO settings(key,value,updated_at) VALUES("+quoteLiteral(key)+","+quoteLiteral(paused[key])+",CURRENT_TIMESTAMP) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value")
		if err != nil {
			return err
		}
	}
	return nil
}

// CompareDatabase checks old column definitions and full old values, not just
// account counts. A column added in an earlier release is never ignored later.
func CompareDatabase(before, after DatabaseSnapshot, manifest Manifest, private bool) error {
	policy := manifest.Validation
	for table, original := range before.Tables {
		actual, ok := after.Tables[table]
		if !ok {
			return errors.New("an original database table was removed")
		}
		columns := map[string]Column{}
		for _, column := range actual.Columns {
			columns[column.Name] = column
		}
		oldNames := map[string]bool{}
		for _, column := range original.Columns {
			if actualColumn, ok := columns[column.Name]; !ok || actualColumn != column {
				return errors.New("an original column definition changed")
			}
			oldNames[column.Name] = true
		}
		for name, column := range columns {
			if oldNames[name] {
				continue
			}
			rule, declared := policy.AddedColumns[table][name]
			if !declared || column.Type != rule.Type || column.Nullable != rule.Nullable || column.Default != rule.Default {
				return errors.New("an undeclared column was added")
			}
		}
		for name := range policy.AddedColumns[table] {
			if _, exists := columns[name]; !exists {
				return errors.New("a declared new column is missing")
			}
		}
		if table != "schema_migrations" && table != "settings" && table != "ops_system_logs" && (actual.Count != original.Count || actual.Digest != original.Digest) {
			return errors.New("migration changed original business data")
		}
	}
	for table, actual := range after.Tables {
		if _, old := before.Tables[table]; old {
			continue
		}
		count, declared := policy.NewTables[table]
		if !declared || actual.Count != count {
			return errors.New("an undeclared or unexpected new table was found")
		}
	}
	for table := range policy.NewTables {
		if _, ok := after.Tables[table]; !ok {
			return errors.New("a declared new table is missing")
		}
	}
	for name, checksum := range before.Migrations {
		if after.Migrations[name] != checksum {
			return errors.New("a historical migration checksum changed")
		}
	}
	if len(after.Migrations) != len(manifest.Migrations) {
		return errors.New("database migrations differ from the verified release catalog")
	}
	for name, checksum := range manifest.Migrations {
		// Upstream accepts specific historical checksums for migrations it once
		// edited. Preserve the already applied checksum; only new migrations must
		// match the current source file. The target's migration runner separately
		// validates its historical compatibility catalog on startup.
		if original, exists := before.Migrations[name]; exists {
			checksum = original
		}
		if after.Migrations[name] != checksum {
			return errors.New("database migrations differ from the verified release catalog")
		}
	}
	expected := map[string]string{}
	for key, value := range before.Settings {
		expected[key] = value
	}
	if private {
		paused, err := pausedSettings(expected)
		if err != nil {
			return err
		}
		expected = paused
	}
	for key, value := range policy.Settings {
		if ProtectedSetting(key) {
			return errors.New("protected authentication settings changed")
		}
		// Preserve a setting already customized after an earlier migration.
		// A declared migration may supply a new value, but need not reapply it.
		if original, existed := expected[key]; !existed || after.Settings[key] != original {
			expected[key] = value
		}
	}
	if !reflect.DeepEqual(expected, after.Settings) {
		return errors.New("undeclared settings or authentication changes were found")
	}
	return nil
}

func (e *Engine) checkAddedValues(ctx context.Context, container Container, before DatabaseSnapshot, policy ValidationPolicy) error {
	for table, columns := range policy.AddedColumns {
		if _, ok := before.Tables[table]; !ok {
			return errors.New("added column policy refers to a missing original table")
		}
		for name, rule := range columns {
			alreadyPresent := false
			for _, column := range before.Tables[table].Columns {
				if column.Name == name {
					alreadyPresent = true
					break
				}
			}
			if alreadyPresent {
				continue
			}
			value, err := e.pg(ctx, container, "SELECT COUNT(*) FROM public."+quoteIdentifier(table)+" WHERE COALESCE(to_jsonb("+quoteIdentifier(name)+"),'null'::jsonb) IS DISTINCT FROM "+quoteLiteral(string(rule.Value))+"::jsonb")
			if err != nil {
				return err
			}
			if value != "0" {
				return errors.New("a new column contains unexpected values")
			}
		}
	}
	return nil
}

func (e *Engine) readonlyKey(ctx context.Context, container Container) (string, error) {
	return e.pg(ctx, container, "SELECT a.key FROM api_keys a JOIN users u ON u.id=a.user_id WHERE a.status='active' AND u.status='active' AND a.deleted_at IS NULL AND u.deleted_at IS NULL ORDER BY a.id LIMIT 1")
}

func databaseRowCount(snapshot DatabaseSnapshot) int64 {
	var count int64
	for _, table := range snapshot.Tables {
		count += table.Count
	}
	return count
}

func validateRestoredDatabase(before, restored DatabaseSnapshot) error {
	if !reflect.DeepEqual(before, restored) {
		return fmt.Errorf("restored database does not match all %d original rows", databaseRowCount(before))
	}
	return nil
}
