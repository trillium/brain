"""MCP project registry and resolver.

Lightweight durable awareness of every project for the communication/routing
agent: foreground (lightweight context for current work) vs backlog (searched
on demand; matches explicitly report their backlog origin).

Binding bootstrap: every seeded project starts in BACKLOG; foreground begins
empty unless the state file explicitly promotes entries. The seed source is
the Project Store (``project`` CLI / ``~/data/projects/.beads/``,
prefix ``project-``). Mentioning a backlog project never promotes it; only
creating a bead associated with it (via the ``project`` hint on create, or an
explicit promote) does. Demotion is never automatic and requires explicit
human confirmation.

Seed data is always re-read from the Project Store; the state file only
carries overlays (foreground set, alias additions, promotion history), so
registry state stays small and never duplicates full project data.
"""

from __future__ import annotations

import json
import logging
import os
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

logger = logging.getLogger(__name__)

STATE_VERSION = 1
FOREGROUND = "foreground"
BACKLOG = "backlog"

DESCRIPTION_MAX_LEN = 280


def _utcnow_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def _short_description(text: str | None) -> str:
    """Collapse a project bead body to one routing-sufficient line."""
    if not text:
        return ""
    single = re.sub(r"\s+", " ", text).strip()
    if len(single) > DESCRIPTION_MAX_LEN:
        return single[: DESCRIPTION_MAX_LEN - 3].rstrip() + "..."
    return single


@dataclass
class ProjectEntry:
    """Routing-sufficient view of one project."""

    project_id: str
    name: str
    aliases: list[str] = field(default_factory=list)
    state: str = BACKLOG
    description: str = ""
    stores: list[str] = field(default_factory=list)
    promoted_at: str | None = None
    promote_reason: str | None = None

    @property
    def from_backlog(self) -> bool:
        return self.state == BACKLOG

    def to_dict(self) -> dict[str, Any]:
        return {
            "project_id": self.project_id,
            "name": self.name,
            "aliases": list(self.aliases),
            "state": self.state,
            "from_backlog": self.from_backlog,
            "description": self.description,
            "stores": list(self.stores),
            "promoted_at": self.promoted_at,
            "promote_reason": self.promote_reason,
        }


def default_state_path() -> str:
    override = os.environ.get("BEADS_MCP_PROJECT_REGISTRY_PATH")
    if override:
        return override
    return os.path.join(
        os.path.expanduser("~"),
        ".local",
        "share",
        "beads-mcp",
        "project-registry.json",
    )


def _project_store_dir() -> str:
    override = os.environ.get("BEADS_PROJECT_STORE_DIR")
    if override:
        return override
    return os.path.join(os.path.expanduser("~"), "data", "projects")


def _project_cli() -> str:
    return os.environ.get("BEADS_PROJECT_CLI", "project")


def _run_json(args: list[str], cwd: str | None) -> list[dict[str, Any]] | None:
    try:
        result = subprocess.run(
            args,
            cwd=cwd,
            capture_output=True,
            text=True,
            check=False,
            stdin=subprocess.DEVNULL,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        logger.debug("project seed command failed %s: %s", args, exc)
        return None
    if result.returncode != 0:
        logger.debug("project seed command %s exited %s: %s", args, result.returncode, result.stderr[:200])
        return None
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError:
        return None
    return data if isinstance(data, list) else None


def load_seed_projects() -> list[dict[str, Any]]:
    """Read raw project rows from the Project Store (best-effort, never raises).

    Tries the ``project`` CLI first, then ``bd`` rooted at the project store
    dir. Returns [] when the store is unreachable so the registry degrades to
    its persisted overlays rather than failing the request.
    """
    store_dir = _project_store_dir()
    rows = _run_json([_project_cli(), "list", "--json"], cwd=store_dir)
    if rows is not None:
        return rows
    bd = shutil.which("bd")
    if bd and os.path.isdir(store_dir):
        rows = _run_json([bd, "list", "--json"], cwd=store_dir)
        if rows is not None:
            return rows
    return []


class ProjectRegistry:
    """Seed + overlay registry with foreground/backlog states."""

    def __init__(
        self,
        seed: list[dict[str, Any]] | None = None,
        state_path: str | None = None,
    ) -> None:
        self.state_path = state_path or default_state_path()
        self._overrides: dict[str, dict[str, Any]] = {}
        self._history: list[dict[str, Any]] = []
        self._load_state()
        raw_seed = seed if seed is not None else load_seed_projects()
        self._entries: dict[str, ProjectEntry] = {}
        for row in raw_seed:
            entry = self._entry_from_row(row)
            if entry is not None:
                self._entries[entry.project_id] = entry
        # Persisted overlays can revive entries whose seed row vanished
        # (e.g. store unreachable this request): keep them, still searchable.
        for pid in self._overrides:
            if pid not in self._entries:
                ov = self._overrides[pid]
                self._entries[pid] = ProjectEntry(
                    project_id=pid,
                    name=ov.get("name", pid),
                    aliases=list(ov.get("aliases_add", [])),
                    state=ov.get("state", BACKLOG),
                    description=ov.get("description", ""),
                    stores=list(ov.get("stores", [])),
                    promoted_at=ov.get("promoted_at"),
                    promote_reason=ov.get("promote_reason"),
                )

    # -- construction ----------------------------------------------------

    def _entry_from_row(self, row: dict[str, Any]) -> ProjectEntry | None:
        pid = str(row.get("id", "")).strip()
        if not pid:
            return None
        ov = self._overrides.get(pid, {})
        extra_aliases = [str(a) for a in ov.get("aliases_add", [])]
        return ProjectEntry(
            project_id=pid,
            name=str(row.get("title") or ov.get("name") or pid),
            aliases=extra_aliases,
            state=ov.get("state", BACKLOG),
            description=ov.get("description") or _short_description(str(row.get("description") or "")),
            stores=list(ov.get("stores", [])),
            promoted_at=ov.get("promoted_at"),
            promote_reason=ov.get("promote_reason"),
        )

    # -- state file -------------------------------------------------------

    def _load_state(self) -> None:
        try:
            with open(self.state_path) as f:
                data = json.load(f)
        except (OSError, json.JSONDecodeError, ValueError):
            return
        if not isinstance(data, dict):
            return
        overrides = data.get("overrides")
        if isinstance(overrides, dict):
            self._overrides = {str(k): v for k, v in overrides.items() if isinstance(v, dict)}
        history = data.get("history")
        if isinstance(history, list):
            self._history = [h for h in history if isinstance(h, dict)]

    def _save_state(self) -> None:
        payload = {"version": STATE_VERSION, "overrides": self._overrides, "history": self._history[-200:]}
        try:
            os.makedirs(os.path.dirname(self.state_path) or ".", exist_ok=True)
            fd, tmp = tempfile.mkstemp(dir=os.path.dirname(self.state_path) or None, suffix=".tmp")
            try:
                with os.fdopen(fd, "w") as f:
                    json.dump(payload, f, indent=2)
                os.replace(tmp, self.state_path)
            except BaseException:
                with suppress_os_error():
                    os.unlink(tmp)
                raise
        except OSError as exc:
            logger.warning("project registry state not persisted to %s: %s", self.state_path, exc)

    def _override(self, pid: str) -> dict[str, Any]:
        return self._overrides.setdefault(pid, {})

    def _record(self, event: str, pid: str, reason: str | None = None) -> None:
        self._history.append(
            {"at": _utcnow_iso(), "event": event, "project_id": pid, "reason": reason or ""}
        )

    # -- reads ------------------------------------------------------------

    def list_projects(self, state: str | None = None, limit: int = 100) -> list[ProjectEntry]:
        entries = sorted(self._entries.values(), key=lambda e: e.project_id)
        if state in (FOREGROUND, BACKLOG):
            entries = [e for e in entries if e.state == state]
        return entries[: max(1, limit)]

    def search(self, query: str, state: str | None = None, limit: int = 20) -> list[ProjectEntry]:
        q = query.strip().lower()
        if not q:
            return self.list_projects(state=state, limit=limit)
        scored: list[tuple[int, ProjectEntry]] = []
        for entry in self._entries.values():
            if state in (FOREGROUND, BACKLOG) and entry.state != state:
                continue
            score = _match_score(q, entry)
            if score > 0:
                scored.append((score, entry))
        scored.sort(key=lambda item: (-item[0], item[1].project_id))
        return [entry for _, entry in scored[: max(1, limit)]]

    def resolve(self, text: str, limit: int = 5) -> list[ProjectEntry]:
        """Resolve natural-language work text against the registry.

        Read-only: resolving never changes state, so merely mentioning a
        backlog project leaves it in backlog. Callers surface ``from_backlog``
        on each hit so downstream routing knows the match origin.
        """
        return self.search(text, limit=limit)

    def foreground_context(self) -> list[dict[str, Any]]:
        """Lightweight foreground dump for injection into current work context."""
        return [e.to_dict() for e in self.list_projects(state=FOREGROUND, limit=100)]

    # -- maintenance (agent-authorized) ------------------------------------

    def _require(self, pid: str) -> ProjectEntry:
        entry = self._entries.get(pid)
        if entry is None:
            raise KeyError(f"unknown project {pid!r}; refresh the registry seed first")
        return entry

    def alias_add(self, pid: str, alias: str) -> ProjectEntry:
        entry = self._require(pid)
        alias = alias.strip()
        if not alias:
            raise ValueError("alias must not be empty")
        haystack = [entry.project_id.lower(), entry.name.lower()] + [a.lower() for a in entry.aliases]
        if alias.lower() not in haystack:
            entry.aliases.append(alias)
            ov = self._override(pid)
            aliases_add = list(ov.get("aliases_add", [])) + [alias]
            ov["aliases_add"] = aliases_add
            self._record("alias-add", pid, alias)
            self._save_state()
        return entry

    def alias_remove(self, pid: str, alias: str) -> ProjectEntry:
        entry = self._require(pid)
        lowered = alias.strip().lower()
        entry.aliases = [a for a in entry.aliases if a.lower() != lowered]
        ov = self._override(pid)
        ov["aliases_add"] = [a for a in ov.get("aliases_add", []) if str(a).lower() != lowered]
        self._record("alias-remove", pid, alias)
        self._save_state()
        return entry

    def promote(self, pid: str, reason: str) -> ProjectEntry:
        """Move a backlog project to foreground, recording when/why."""
        entry = self._require(pid)
        if not reason.strip():
            raise ValueError("promotion requires a reason (when/why is recorded)")
        entry.state = FOREGROUND
        entry.promoted_at = _utcnow_iso()
        entry.promote_reason = reason.strip()
        ov = self._override(pid)
        ov["state"] = FOREGROUND
        ov["promoted_at"] = entry.promoted_at
        ov["promote_reason"] = entry.promote_reason
        self._record("promote", pid, reason.strip())
        self._save_state()
        return entry

    def demote(self, pid: str, reason: str, human_confirmed: bool = False) -> ProjectEntry:
        """Move a foreground project back to backlog.

        Never automatic: requires explicit human intent via ``human_confirmed``.
        """
        entry = self._require(pid)
        if not human_confirmed:
            raise PermissionError(
                f"refusing to demote {pid!r} without explicit human intent "
                "(pass human_confirmed=True after the human asked for the demotion)"
            )
        if not reason.strip():
            raise ValueError("demotion requires a reason (when/why is recorded)")
        entry.state = BACKLOG
        ov = self._override(pid)
        ov["state"] = BACKLOG
        self._record("demote", pid, reason.strip())
        self._save_state()
        return entry

    def maybe_promote_on_create(
        self,
        project_hint: str | None,
        labels: list[str] | None = None,
        reason_prefix: str = "auto: bead created",
    ) -> ProjectEntry | None:
        """Auto-promote the backlog project a new bead was filed under.

        Merely mentioning a project never promotes; filing work under it does.
        Returns the promoted entry, or None when no promotion applied.
        """
        candidates: list[str] = []
        if project_hint and project_hint.strip():
            candidates.append(project_hint.strip())
        for label in labels or []:
            if label.startswith("project-"):
                candidates.append(label)
        for candidate in candidates:
            entry = self._entries.get(candidate)
            if entry is None:
                hits = self.search(candidate, limit=1)
                entry = hits[0] if hits and _match_score(candidate.lower(), hits[0]) >= 100 else None
            if entry is not None and entry.state == BACKLOG:
                return self.promote(entry.project_id, f"{reason_prefix} for {entry.project_id}")
            if entry is not None:
                return None
        return None


def _match_score(q: str, entry: ProjectEntry) -> int:
    """Rank a query against one entry; 0 means no match."""
    q = q.strip().lower()
    if not q:
        return 0
    if q == entry.project_id.lower():
        return 300
    if q == entry.name.lower():
        return 200
    for alias in entry.aliases:
        if q == alias.lower():
            return 200
    if entry.project_id.lower().startswith(q):
        return 150
    for alias in entry.aliases:
        if q in alias.lower():
            return 120
    if q in entry.name.lower():
        return 100
    tokens = [t for t in re.split(r"[^a-z0-9]+", q) if len(t) > 2]
    haystack = f"{entry.project_id} {entry.name} {' '.join(entry.aliases)} {entry.description}".lower()
    token_hits = sum(1 for t in tokens if t in haystack)
    if tokens and token_hits == len(tokens) and token_hits > 0:
        return 50 + token_hits
    if q in haystack:
        return 10
    return 0


class suppress_os_error:
    """Context manager suppressing OSError on cleanup paths."""

    def __enter__(self) -> suppress_os_error:
        return self

    def __exit__(self, *exc: object) -> bool:
        return True


if __name__ == "__main__":  # pragma: no cover - manual smoke check
    registry = ProjectRegistry()
    fg = len(registry.list_projects(state=FOREGROUND))
    bl = len(registry.list_projects(state=BACKLOG))
    print(f"foreground={fg} backlog={bl}")
    sys.exit(0)
