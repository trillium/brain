-- Migration 0052 down: drop the UNIQUE index on `issues.slug`.
--
-- Gated on INFORMATION_SCHEMA so the down migration is safe to re-run
-- and safe to apply on databases where 0052.up never landed.

SET @has_idx = (
    SELECT IF(COUNT(*) > 0, 1, 0)
    FROM INFORMATION_SCHEMA.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME   = 'issues'
      AND INDEX_NAME   = 'idx_issues_slug_unique'
);
SET @sql = IF(@has_idx = 1,
    'DROP INDEX idx_issues_slug_unique ON issues',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
