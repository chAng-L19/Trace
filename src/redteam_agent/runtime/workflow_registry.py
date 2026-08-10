from __future__ import annotations

import re
import tomllib
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

from .models import ActionSpec, GoalContract, WorkflowSpec


TOKEN_RE = re.compile(r"[a-z0-9][a-z0-9_.+-]*|[\u4e00-\u9fff]+", re.IGNORECASE)
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
PROFILE_SCHEMA_VERSION = 2
PROFILE_FIELDS = frozenset(
    {
        "id",
        "version",
        "name",
        "description",
        "match_tags",
        "coverage_focus",
        "capability_hints",
        "artifact_extensions",
        "negative_controls",
    }
)


def _tag_matches(normalized: str, tokens: set[str], tag: str) -> bool:
    if any("\u4e00" <= character <= "\u9fff" for character in tag) or " " in tag or "-" in tag:
        return tag in normalized
    return tag in tokens


def _strings(value: Any, *, field: str, profile_id: str) -> tuple[str, ...]:
    if value is None:
        return ()
    if not isinstance(value, list) or any(not isinstance(item, str) or not item.strip() for item in value):
        raise ValueError(f"workflow_profile_field_invalid:{profile_id}:{field}")
    return tuple(dict.fromkeys(item.strip() for item in value))


@dataclass(frozen=True)
class WorkflowProfile:
    profile_id: str
    version: int
    name: str
    description: str
    match_tags: tuple[str, ...]
    coverage_focus: tuple[str, ...]
    capability_hints: tuple[str, ...]
    artifact_extensions: tuple[str, ...]
    negative_controls: tuple[str, ...]

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any], path: Path) -> "WorkflowProfile":
        unknown = set(payload) - PROFILE_FIELDS
        profile_id = str(payload.get("id") or "").strip()
        if unknown:
            raise ValueError(f"workflow_profile_fields_forbidden:{profile_id or path}:{sorted(unknown)}")
        if not profile_id or not IDENTIFIER_RE.fullmatch(profile_id):
            raise ValueError(f"workflow_profile_id_invalid:{profile_id or path}")
        return cls(
            profile_id=profile_id,
            version=max(1, int(payload.get("version") or 1)),
            name=str(payload.get("name") or profile_id).strip(),
            description=str(payload.get("description") or "").strip(),
            match_tags=tuple(
                item.casefold()
                for item in _strings(payload.get("match_tags"), field="match_tags", profile_id=profile_id)
            ),
            coverage_focus=_strings(payload.get("coverage_focus"), field="coverage_focus", profile_id=profile_id),
            capability_hints=_strings(payload.get("capability_hints"), field="capability_hints", profile_id=profile_id),
            artifact_extensions=_strings(
                payload.get("artifact_extensions"), field="artifact_extensions", profile_id=profile_id
            ),
            negative_controls=_strings(
                payload.get("negative_controls"), field="negative_controls", profile_id=profile_id
            ),
        )

    def overlay(self) -> dict[str, object]:
        return {
            "profile_id": self.profile_id,
            "coverage_focus": list(self.coverage_focus),
            "capability_hints": list(self.capability_hints),
            "artifact_extensions": list(self.artifact_extensions),
            "negative_controls": list(self.negative_controls),
        }


class WorkflowRegistry:
    """Load one execution DAG and apply data-only domain overlays.

    Profiles can improve planning context, but they cannot create actions,
    change dependencies, require fixed tools, weaken verifiers, or alter the
    terminal contract.  This keeps execution semantics singular and auditable.
    """

    def __init__(self, roots: Iterable[Path] | None = None) -> None:
        default_root = Path(__file__).resolve().parent.parent / "workflows"
        self.roots = tuple(roots or (default_root,))
        self._base: WorkflowSpec | None = None
        self._profiles: dict[str, WorkflowProfile] = {}

    def load(self, *, refresh: bool = False) -> tuple[WorkflowSpec, ...]:
        if self._base is not None and not refresh:
            return (self._base,)
        workflows: dict[str, WorkflowSpec] = {}
        profile_documents: list[tuple[Path, Mapping[str, Any]]] = []
        for root in self.roots:
            if not root.is_dir():
                continue
            for path in sorted(root.glob("*.toml")):
                payload = tomllib.loads(path.read_text(encoding="utf-8-sig"))
                if "profile_schema_version" in payload:
                    profile_documents.append((path, payload))
                    continue
                workflow = WorkflowSpec.from_dict(payload)
                self._validate(workflow, path)
                if workflow.workflow_id in workflows:
                    raise ValueError(f"duplicate_workflow_id:{workflow.workflow_id}")
                workflows[workflow.workflow_id] = workflow
        if set(workflows) != {"generic-adaptive"}:
            raise ValueError(f"single_generic_workflow_required:{sorted(workflows)}")
        profiles: dict[str, WorkflowProfile] = {}
        for path, document in profile_documents:
            if document.get("profile_schema_version") != PROFILE_SCHEMA_VERSION:
                raise ValueError(f"workflow_profile_schema_unsupported:{path}")
            if str(document.get("template") or "generic-adaptive") != "generic-adaptive":
                raise ValueError(f"workflow_profile_template_invalid:{path}")
            raw_profiles = document.get("profiles")
            if not isinstance(raw_profiles, list):
                raise ValueError(f"workflow_profiles_missing:{path}")
            for raw_profile in raw_profiles:
                if not isinstance(raw_profile, Mapping):
                    raise ValueError(f"workflow_profile_invalid:{path}")
                profile = WorkflowProfile.from_dict(raw_profile, path)
                if profile.profile_id in profiles:
                    raise ValueError(f"workflow_profile_duplicate:{profile.profile_id}")
                profiles[profile.profile_id] = profile
        self._base = workflows["generic-adaptive"]
        self._profiles = profiles
        return (self._base,)

    @property
    def profile_ids(self) -> tuple[str, ...]:
        self.load()
        return tuple(self._profiles)

    def _validate(self, workflow: WorkflowSpec, path: Path) -> None:
        if not workflow.workflow_id or not IDENTIFIER_RE.fullmatch(workflow.workflow_id):
            raise ValueError(f"workflow_id_invalid:{workflow.workflow_id or path}")
        if not workflow.actions:
            raise ValueError(f"workflow_actions_missing:{workflow.workflow_id}")
        action_ids = [action.action_id for action in workflow.actions]
        if any(not IDENTIFIER_RE.fullmatch(action_id) for action_id in action_ids):
            raise ValueError(f"workflow_action_id_invalid:{workflow.workflow_id}")
        if len(action_ids) != len(set(action_ids)):
            raise ValueError(f"workflow_duplicate_action:{workflow.workflow_id}")
        known = set(action_ids)
        for action in workflow.actions:
            if not action.required_capabilities:
                raise ValueError(f"workflow_capability_missing:{workflow.workflow_id}:{action.action_id}")
            if not action.expected_artifact:
                raise ValueError(f"workflow_artifact_missing:{workflow.workflow_id}:{action.action_id}")
            unknown = set(action.depends_on) - known
            if unknown:
                raise ValueError(f"workflow_unknown_dependency:{workflow.workflow_id}:{action.action_id}:{sorted(unknown)}")
        remaining = {action.action_id: set(action.depends_on) for action in workflow.actions}
        while remaining:
            ready = {action_id for action_id, dependencies in remaining.items() if not (dependencies & remaining.keys())}
            if not ready:
                raise ValueError(f"workflow_dependency_cycle:{workflow.workflow_id}")
            for action_id in ready:
                remaining.pop(action_id)

    def _profile_matches(self, goal: GoalContract, *, limit: int) -> tuple[WorkflowProfile, ...]:
        requested = tuple(
            dict.fromkeys(
                item
                for item in (*goal.workflow_hints, goal.workflow_hint)
                if item and item != "generic-adaptive" and item in self._profiles
            )
        )
        if requested:
            return tuple(self._profiles[item] for item in requested[:limit])
        normalized = goal.objective.casefold().replace("_", "-")
        tokens = set(TOKEN_RE.findall(normalized))
        ranked: list[tuple[int, str, WorkflowProfile]] = []
        for profile in self._profiles.values():
            score = sum(1 for tag in profile.match_tags if _tag_matches(normalized, tokens, tag))
            ranked.append((score, profile.profile_id, profile))
        ranked.sort(key=lambda item: (-item[0], item[1]))
        return tuple(item[2] for item in ranked if item[0] > 0)[:limit]

    @staticmethod
    def _merge(profiles: Sequence[WorkflowProfile], field: str) -> list[str]:
        return list(
            dict.fromkeys(value for profile in profiles for value in getattr(profile, field))
        )

    def _apply_profiles(self, base: WorkflowSpec, profiles: Sequence[WorkflowProfile]) -> WorkflowSpec:
        if not profiles:
            return base
        profile_ids = [profile.profile_id for profile in profiles]
        merged = {
            "profile_ids": profile_ids,
            "coverage_focus": self._merge(profiles, "coverage_focus"),
            "capability_hints": self._merge(profiles, "capability_hints"),
            "artifact_extensions": self._merge(profiles, "artifact_extensions"),
            "negative_controls": self._merge(profiles, "negative_controls"),
        }
        action_fields = {
            "surface_map": ("coverage_focus", "capability_hints"),
            "hypothesis_queue": ("coverage_focus", "capability_hints"),
            "reproduction_artifact": ("capability_hints", "artifact_extensions", "negative_controls"),
            "impact_proof": ("artifact_extensions", "negative_controls"),
            "coverage_report": ("coverage_focus", "artifact_extensions", "negative_controls"),
            "cleanup_proof": ("negative_controls",),
            "final_report": ("coverage_focus", "artifact_extensions", "negative_controls"),
        }
        actions: list[ActionSpec] = []
        for action in base.actions:
            fields = action_fields.get(action.expected_artifact, ())
            overlay = {"profile_ids": profile_ids, **{field: merged[field] for field in fields}}
            actions.append(replace(action, parameters={**dict(action.parameters), "profile_overlay": overlay}))
        return replace(
            base,
            description=f"{base.description} Active overlays: {', '.join(profile_ids)}.",
            actions=tuple(actions),
        )

    @staticmethod
    def _apply_prompt_contract(workflow: WorkflowSpec, goal: GoalContract) -> WorkflowSpec:
        """Project the single DAG to the user's lossless execution contract."""

        envelope = goal.intent_envelope if isinstance(goal.intent_envelope, Mapping) else {}
        if envelope.get("action_kind") != "plan" or envelope.get("execution_required") is not False:
            return workflow
        selected: list[ActionSpec] = []
        for action in workflow.actions:
            if action.expected_artifact not in {"surface_map", "hypothesis_queue", "final_report"}:
                continue
            if action.expected_artifact == "final_report":
                action = replace(
                    action,
                    depends_on=("build-hypotheses",),
                    parameters={**dict(action.parameters), "prompt_contract_mode": "plan"},
                )
            selected.append(action)
        allowed_artifacts = {action.expected_artifact for action in selected}
        terminal_predicates = tuple(
            predicate
            for predicate in workflow.terminal_predicates
            if predicate.kind == "workflow_actions_complete"
            or (predicate.kind == "artifact_verified" and predicate.subject in allowed_artifacts)
        )
        return replace(
            workflow,
            description=f"{workflow.description} Prompt contract projection: plan-only.",
            actions=tuple(selected),
            required_artifacts=tuple(
                artifact
                for artifact in workflow.required_artifacts
                if artifact in allowed_artifacts
            ),
            terminal_predicates=terminal_predicates,
        )

    def get(self, workflow_id: str) -> WorkflowSpec:
        self.load()
        assert self._base is not None
        if workflow_id == "generic-adaptive":
            return self._base
        if workflow_id in self._profiles:
            return self._apply_profiles(self._base, (self._profiles[workflow_id],))
        raise KeyError(f"workflow_not_found:{workflow_id}")

    def match(self, goal: GoalContract) -> WorkflowSpec:
        self.load()
        assert self._base is not None
        profiled = self._apply_profiles(
            self._base,
            self._profile_matches(goal, limit=max(1, len(self._profiles))),
        )
        return self._apply_prompt_contract(profiled, goal)

    def match_many(self, goal: GoalContract, *, limit: int = 3) -> tuple[WorkflowSpec, ...]:
        del limit
        return (self.match(goal),)
