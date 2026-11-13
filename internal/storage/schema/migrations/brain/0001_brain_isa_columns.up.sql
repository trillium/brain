-- Migration 0050: Add ISA (Ideal State Artifact) columns to the `issues` table.
--
-- These columns let a single bead carry the lifecycle metadata of an ISA-backed
-- work artifact alongside the existing issue surface (status, priority, etc).
-- The ISA itself is sectioned content stored in the companion `isa_sections`
-- table (migration 0051); these columns hold the small, queryable header.
--
-- Type choices follow the convention already established by `issues`:
--   * identifier-like short strings (slug, enum-ish phase/effort/mode) use
--     VARCHAR(N) — matches existing columns like `status VARCHAR(32)`,
--     `wisp_type VARCHAR(32)`, `external_ref VARCHAR(255)`.
--   * progress is a small counter pair, INT.
--   * timestamps use DATETIME NULL — the spec asks for nullable TIMESTAMPs,
--     and Dolt treats TIMESTAMP and DATETIME interchangeably for storage,
--     but the existing `issues` table standardizes on DATETIME
--     (`started_at`, `closed_at`, `compacted_at`, `last_activity`).
--     DATETIME NULL preserves the "no started time for non-ISA rows" semantics.
--
-- Each ADD COLUMN is guarded by an INFORMATION_SCHEMA check so the migration
-- is safe to re-run. Dolt does not support `ADD COLUMN IF NOT EXISTS`, so the
-- idempotency pattern is the same prepared-DDL guard used by migration 0046
-- (`is_blocked`) and the issues-column guards in 0049.

-- slug
SET @needs_slug = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'slug'
);
SET @sql = IF(@needs_slug = 1,
    'ALTER TABLE issues ADD COLUMN slug VARCHAR(255) DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- isa_phase
SET @needs_isa_phase = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'isa_phase'
);
SET @sql = IF(@needs_isa_phase = 1,
    'ALTER TABLE issues ADD COLUMN isa_phase VARCHAR(32) DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- isa_progress_m
SET @needs_isa_progress_m = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'isa_progress_m'
);
SET @sql = IF(@needs_isa_progress_m = 1,
    'ALTER TABLE issues ADD COLUMN isa_progress_m INT DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- isa_progress_n
SET @needs_isa_progress_n = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'isa_progress_n'
);
SET @sql = IF(@needs_isa_progress_n = 1,
    'ALTER TABLE issues ADD COLUMN isa_progress_n INT DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- isa_effort
SET @needs_isa_effort = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'isa_effort'
);
SET @sql = IF(@needs_isa_effort = 1,
    'ALTER TABLE issues ADD COLUMN isa_effort VARCHAR(8) DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- isa_mode
SET @needs_isa_mode = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'isa_mode'
);
SET @sql = IF(@needs_isa_mode = 1,
    'ALTER TABLE issues ADD COLUMN isa_mode VARCHAR(32) DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- isa_started_at
SET @needs_isa_started_at = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'isa_started_at'
);
SET @sql = IF(@needs_isa_started_at = 1,
    'ALTER TABLE issues ADD COLUMN isa_started_at DATETIME NULL DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- isa_updated_at
SET @needs_isa_updated_at = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'issues'
      AND COLUMN_NAME = 'isa_updated_at'
);
SET @sql = IF(@needs_isa_updated_at = 1,
    'ALTER TABLE issues ADD COLUMN isa_updated_at DATETIME NULL DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
