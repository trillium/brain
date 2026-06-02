-- Migration 0051: Create the `isa_sections` table.
--
-- Stores the sectioned body of an ISA (Ideal State Artifact) keyed by the
-- bead's `issues.id` plus the section name (Problem, Vision, Criteria,
-- Features, Changelog, etc — see PAI/DOCUMENTATION/IsaFormat.md for the
-- twelve-section spec). The header metadata lives on the `issues` row itself
-- (migration 0050: slug, isa_phase, isa_progress_m/n, isa_effort, isa_mode,
-- isa_started_at, isa_updated_at).
--
-- Type choices:
--   * issue_id: VARCHAR(255) to match `issues.id` (the spec text suggested
--     TEXT with a prefix-length PK; VARCHAR(255) gives the same effective
--     primary-key behavior without needing MySQL prefix-length syntax and
--     matches the precedent set by `dependencies.issue_id` and
--     `comments.issue_id`).
--   * section_name: VARCHAR(64) — section names are short canonical labels.
--   * body: LONGTEXT — follows the precedent in migration 0049, which moved
--     all large-content columns to LONGTEXT because TEXT's 65535-byte cap
--     truncates real ISA bodies (embedded base64, large checklists, etc).
--   * updated_at: TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP (per spec).
--
-- Idempotency: CREATE TABLE IF NOT EXISTS is supported by Dolt directly, so
-- no INFORMATION_SCHEMA guard is needed for this migration (the column-level
-- guards in 0050 are required because Dolt does *not* support
-- `ADD COLUMN IF NOT EXISTS`).

CREATE TABLE IF NOT EXISTS isa_sections (
    issue_id     VARCHAR(255) NOT NULL,
    section_name VARCHAR(64)  NOT NULL,
    body         LONGTEXT     NOT NULL,
    updated_at   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (issue_id, section_name)
);
