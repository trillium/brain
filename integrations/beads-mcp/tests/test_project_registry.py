"""Tests for the MCP project registry (foreground/backlog semantics)."""

import pytest

from beads_mcp.project_registry import BACKLOG, FOREGROUND, ProjectRegistry

SEED = [
    {"id": "project-3ha", "title": "beads", "description": "Dolt-backed issue tracker for AI agents."},
    {"id": "project-mf1", "title": "agentic-infra", "description": "Agent infrastructure and fleet work."},
    {"id": "project-y4s", "title": "massage", "description": "Massage booking site and redirects."},
]


def make_registry(tmp_path, seed=None):
    return ProjectRegistry(seed=seed if seed is not None else SEED, state_path=str(tmp_path / "registry.json"))


def test_bootstrap_all_backlog_foreground_empty(tmp_path):
    r = make_registry(tmp_path)
    assert r.list_projects(state=FOREGROUND) == []
    assert len(r.list_projects(state=BACKLOG)) == 3


def test_search_reports_backlog_origin(tmp_path):
    r = make_registry(tmp_path)
    hits = r.search("beads")
    assert hits and hits[0].project_id == "project-3ha"
    assert hits[0].from_backlog is True
    assert hits[0].to_dict()["from_backlog"] is True


def test_resolve_by_alias_and_id(tmp_path):
    r = make_registry(tmp_path)
    r.alias_add("project-3ha", "bd")
    assert r.resolve("bd")[0].project_id == "project-3ha"
    assert r.resolve("project-mf1")[0].project_id == "project-mf1"


def test_mention_does_not_promote(tmp_path):
    r = make_registry(tmp_path)
    r.resolve("beads issue tracker")
    r.search("massage")
    assert r.list_projects(state=FOREGROUND) == []
    assert len(r.list_projects(state=BACKLOG)) == 3


def test_create_hook_promotes_and_records_why(tmp_path):
    r = make_registry(tmp_path)
    promoted = r.maybe_promote_on_create("project-3ha", reason_prefix="auto: bead created")
    assert promoted is not None and promoted.state == FOREGROUND
    assert promoted.promote_reason and "project-3ha" in promoted.promote_reason
    assert promoted.promoted_at
    assert [e.project_id for e in r.list_projects(state=FOREGROUND)] == ["project-3ha"]


def test_create_hook_label_hint_and_no_hint(tmp_path):
    r = make_registry(tmp_path)
    assert r.maybe_promote_on_create(None) is None
    assert r.list_projects(state=FOREGROUND) == []
    promoted = r.maybe_promote_on_create(None, labels=["project-y4s"])
    assert promoted is not None and promoted.project_id == "project-y4s"


def test_demote_requires_human_intent(tmp_path):
    r = make_registry(tmp_path)
    r.promote("project-3ha", "working on it")
    with pytest.raises(PermissionError):
        r.demote("project-3ha", "no longer relevant")
    assert r.list_projects(state=FOREGROUND)[0].project_id == "project-3ha"
    r.demote("project-3ha", "human asked", human_confirmed=True)
    assert r.list_projects(state=FOREGROUND) == []


def test_alias_maintenance_persists(tmp_path):
    state = str(tmp_path / "registry.json")
    r = ProjectRegistry(seed=SEED, state_path=state)
    r.alias_add("project-3ha", "zz-bead-alias")
    r.promote("project-mf1", "claimed work")
    r2 = ProjectRegistry(seed=SEED, state_path=state)
    assert r2.resolve("zz-bead-alias")[0].project_id == "project-3ha"
    assert [e.project_id for e in r2.list_projects(state=FOREGROUND)] == ["project-mf1"]
    r2.alias_remove("project-3ha", "zz-bead-alias")
    assert all(e.project_id != "project-3ha" for e in r2.resolve("zz-bead-alias"))


def test_unknown_project_errors(tmp_path):
    r = make_registry(tmp_path)
    with pytest.raises(KeyError):
        r.promote("project-nope", "reason")
    with pytest.raises(ValueError):
        r.promote("project-3ha", "  ")


def test_seed_unreachable_degrades_to_empty(tmp_path):
    r = ProjectRegistry(seed=[], state_path=str(tmp_path / "r.json"))
    assert r.list_projects() == []
    assert r.search("anything") == []
