-- Migration 0052: Add a UNIQUE index on `issues.slug`.
--
-- F1a (migration 0050) added the nullable `slug` column. F1d (the ISA
-- substrate) requires that database-level uniqueness be enforced so the
-- `brain new --kind=isa --slug=…` verb can rely on error-on-collision
-- semantics rather than a TOCTOU pre-check.
--
-- MySQL / Dolt allow multiple NULL values in a nullable UNIQUE index
-- (NULL is treated as distinct from every other NULL), so non-ISA rows
-- that leave `slug` NULL are unaffected. Only non-NULL slugs are
-- forced to be unique across the table.
--
-- Idempotency follows the same INFORMATION_SCHEMA-gated PREPARE/EXECUTE
-- pattern established by migrations 0046 and 0050 — Dolt does not
-- support `CREATE UNIQUE INDEX IF NOT EXISTS`.

SET @needs_idx = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME   = 'issues'
      AND INDEX_NAME   = 'idx_issues_slug_unique'
);
SET @sql = IF(@needs_idx = 1,
    'CREATE UNIQUE INDEX idx_issues_slug_unique ON issues (slug)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
