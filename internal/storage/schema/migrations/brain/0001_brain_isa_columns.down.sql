-- Reverse of 0050: drop the eight ISA columns added to `issues`.
-- Down migrations are documentation/manual-rollback only — they are not
-- auto-applied by the schema runtime (see schema.go: only `*.up.sql` is
-- embedded and executed by `migrationSource.migrate`).

ALTER TABLE issues DROP COLUMN isa_updated_at;
ALTER TABLE issues DROP COLUMN isa_started_at;
ALTER TABLE issues DROP COLUMN isa_mode;
ALTER TABLE issues DROP COLUMN isa_effort;
ALTER TABLE issues DROP COLUMN isa_progress_n;
ALTER TABLE issues DROP COLUMN isa_progress_m;
ALTER TABLE issues DROP COLUMN isa_phase;
ALTER TABLE issues DROP COLUMN slug;
