-- Reverse of 0051: drop the `isa_sections` table.
-- Down migrations are documentation/manual-rollback only — they are not
-- auto-applied by the schema runtime.

DROP TABLE IF EXISTS isa_sections;
