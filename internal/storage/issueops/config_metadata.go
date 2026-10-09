package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SetConfigInTx sets a configuration value within an existing transaction.
// Normalizes issue_prefix by stripping trailing hyphens.
//
// On the unified database with a wrapper-pinned namespace the value is
// written to the re-keyed brain_unified_config under that namespace, so the
// store addresses its own settings instead of the template store's shared
// key space (docs/design/brain-single-database.md, "What changes shape").
func SetConfigInTx(ctx context.Context, tx DBTX, key, value string) error {
	if key == "issue_prefix" {
		value = strings.TrimSuffix(value, "-")
	}
	sc := UnifiedScopeForTx(ctx, tx)
	var query string
	var args []any
	if sc.Enabled() {
		query = "REPLACE INTO " + sc.name(utConfig) + " (`store`, `key`, value) VALUES (?, ?, ?)"
		args = []any{sc.Store, key, value}
	} else {
		query = "REPLACE INTO config (`key`, value) VALUES (?, ?)"
		args = []any{key, value}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("set config %s: %w", key, err)
	}
	return nil
}

// getConfigValueInTx is the scoped read of one config row. Returns
// ("", sql.ErrNoRows) when the key does not exist, mirroring
// GetConfigInTx's documented contract so existing error checks keep working.
func getConfigValueInTx(ctx context.Context, tx DBTX, key string) (string, error) {
	sc := UnifiedScopeForTx(ctx, tx)
	var value string
	err := tx.QueryRowContext(ctx, sc.configReadQuery(), append(sc.configReadArgs(), key)...).Scan(&value)
	if err == sql.ErrNoRows {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("get config %s: %w", key, err)
	}
	return value, nil
}

// GetConfigInTx retrieves a configuration value within an existing transaction.
// Returns ("", nil) if the key does not exist.
func GetConfigInTx(ctx context.Context, tx DBTX, key string) (string, error) {
	value, err := getConfigValueInTx(ctx, tx, key)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// GetAllConfigInTx retrieves all configuration key-value pairs within an
// existing transaction — the namespace's pairs when scoped.
func GetAllConfigInTx(ctx context.Context, tx DBTX) (map[string]string, error) {
	sc := UnifiedScopeForTx(ctx, tx)
	query := "SELECT `key`, value FROM config"
	args := []any{}
	if sc.Enabled() {
		query = "SELECT `key`, value FROM " + sc.name(utConfig) + " WHERE `store` = ?"
		args = []any{sc.Store}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get all config: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("get all config: scan: %w", err)
		}
		result[k] = v
	}
	return result, rows.Err()
}

// SetMetadataInTx sets a metadata value within an existing transaction.
func SetMetadataInTx(ctx context.Context, tx DBTX, key, value string) error {
	sc := UnifiedScopeForTx(ctx, tx)
	var query string
	var args []any
	if sc.Enabled() {
		query = "REPLACE INTO " + sc.name(utMetadata) + " (`store`, `key`, value) VALUES (?, ?, ?)"
		args = []any{sc.Store, key, value}
	} else {
		query = "REPLACE INTO metadata (`key`, value) VALUES (?, ?)"
		args = []any{key, value}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("set metadata %s: %w", key, err)
	}
	return nil
}

// getMetadataValueInTx is the scoped read of one metadata row. Returns
// sql.ErrNoRows when absent so callers that check for it keep working.
func getMetadataValueInTx(ctx context.Context, tx DBTX, key string) (string, error) {
	sc := UnifiedScopeForTx(ctx, tx)
	query := "SELECT value FROM " + sc.name(utMetadata) + " WHERE `key` = ?"
	args := []any{key}
	if sc.Enabled() {
		query = "SELECT value FROM " + sc.name(utMetadata) + " WHERE `store` = ? AND `key` = ?"
		args = []any{sc.Store, key}
	}
	var value string
	err := tx.QueryRowContext(ctx, query, args...).Scan(&value)
	if err != nil {
		return "", fmt.Errorf("get metadata %s: %w", key, err)
	}
	return value, nil
}

// GetMetadataInTx retrieves a metadata value within an existing transaction.
// Returns ("", nil) if the key does not exist.
func GetMetadataInTx(ctx context.Context, tx DBTX, key string) (string, error) {
	value, err := getMetadataValueInTx(ctx, tx, key)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// SetLocalMetadataInTx sets a value in the dolt-ignored local_metadata table
// within an existing transaction. Used for clone-local state that should not
// generate merge conflicts (tip timestamps, version stamps, sync cursors).
func SetLocalMetadataInTx(ctx context.Context, tx DBTX, key, value string) error {
	sc := UnifiedScopeForTx(ctx, tx)
	var query string
	var args []any
	if sc.Enabled() {
		query = "REPLACE INTO " + sc.name(utLocalMetadata) + " (`store`, `key`, value) VALUES (?, ?, ?)"
		args = []any{sc.Store, key, value}
	} else {
		query = "REPLACE INTO local_metadata (`key`, value) VALUES (?, ?)"
		args = []any{key, value}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("set local metadata %s: %w", key, err)
	}
	return nil
}

// getLocalMetadataValueInTx is the scoped read of one local_metadata row.
func getLocalMetadataValueInTx(ctx context.Context, tx DBTX, key string) (string, error) {
	sc := UnifiedScopeForTx(ctx, tx)
	query := "SELECT value FROM " + sc.name(utLocalMetadata) + " WHERE `key` = ?"
	args := []any{key}
	if sc.Enabled() {
		query = "SELECT value FROM " + sc.name(utLocalMetadata) + " WHERE `store` = ? AND `key` = ?"
		args = []any{sc.Store, key}
	}
	var value string
	err := tx.QueryRowContext(ctx, query, args...).Scan(&value)
	if err != nil {
		return "", fmt.Errorf("get local metadata %s: %w", key, err)
	}
	return value, nil
}

// GetLocalMetadataInTx retrieves a value from the dolt-ignored local_metadata
// table within an existing transaction. Returns ("", nil) if the key does not exist.
func GetLocalMetadataInTx(ctx context.Context, tx DBTX, key string) (string, error) {
	value, err := getLocalMetadataValueInTx(ctx, tx, key)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}
