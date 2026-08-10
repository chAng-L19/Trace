from __future__ import annotations

import hashlib
import json
import re
from collections import deque
from dataclasses import dataclass, fields
from typing import Sequence


REWRITE_VERSION = "lossless-execution-v2"

# Boundaries are intentionally syntax-only.  Rewriting must never rely on a
# model or a lossy natural-language transformation: each derived field remains
# an index over the source text, whose original bytes are stored below.
_CLAUSE_BOUNDARY = re.compile(
    r"(?:\r?\n)+|[;\uFF1B\u3002]+|\s+(?:and then|then|plus|and)\s+|"
    r"[,\uFF0C](?=\s*(?:\u7136\u540E|\u5E76\u4E14|\u540C\u65F6|\u6700\u540E|and\b|then\b))",
    re.IGNORECASE,
)
_TOKEN = re.compile(r"[a-z][a-z0-9_+-]*|[\u4e00-\u9fff]{1,12}", re.IGNORECASE)

_SCENE_MARKERS: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("model-security", ("jailbreak", "prompt injection", "prompt-injection", "llm", "\u8d8a\u72f1", "\u63d0\u793a\u8bcd", "\u6a21\u578b")),
    ("reverse", ("reverse", "binary", "firmware", "apk", "\u9006\u5411", "\u53cd\u7f16\u8bd1", "\u4e8c\u8fdb\u5236", "\u56fa\u4ef6")),
    ("audit", ("code audit", "source review", "\u4ee3\u7801\u5ba1\u8ba1", "\u6e90\u7801\u5ba1\u8ba1", "\u9759\u6001\u5206\u6790")),
    ("ir", ("incident response", "forensic", "malware", "\u5e94\u6025", "\u53d6\u8bc1", "\u6076\u610f\u6837\u672c", "ioc")),
    ("crypto", ("crypto", "cryptography", "rsa", "aes", "\u5bc6\u7801", "\u52a0\u5bc6", "\u7b7e\u540d")),
    ("ctf", ("ctf", "flag", "challenge", "\u593a\u65d7")),
    ("pentest", ("pentest", "red team", "\u6e17\u900f", "\u7ea2\u961f", "\u63a2\u6d4b", "\u53d1\u5305")),
    ("vuln", ("vulnerability", "exploit", "poc", "sqli", "sql injection", "\u6f0f\u6d1e", "\u590d\u73b0", "\u6ce8\u5165")),
    ("tool", ("scanner", "fuzzer", "automation", "\u5de5\u5177", "\u811a\u672c", "\u81ea\u52a8\u5316")),
)

_VERBS: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("acquire", ("download", "fetch", "obtain", "\u83b7\u53d6", "\u4e0b\u8f7d", "\u6536\u96c6")),
    ("inspect", ("inspect", "analyze", "audit", "review", "\u5206\u6790", "\u5ba1\u67e5", "\u5ba1\u8ba1", "\u68c0\u67e5")),
    ("discover", ("scan", "enumerate", "recon", "\u63a2\u6d4b", "\u626b\u63cf", "\u679a\u4e3e", "\u4fa6\u5bdf")),
    ("transform", ("modify", "patch", "rewrite", "integrate", "\u6539\u9020", "\u4fee\u6539", "\u91cd\u5199", "\u96c6\u6210", "\u6574\u5408")),
    ("implement", ("implement", "build", "create", "develop", "get it done", "make the changes", "\u5b9e\u73b0", "\u6784\u5efa", "\u521b\u5efa", "\u5f00\u53d1", "\u7f16\u5199", "\u641e\u5b9a", "\u5b9e\u65bd")),
    ("execute", ("execute", "run", "launch", "do it", "just do it", "go ahead", "proceed", "start work", "start working", "\u53d1\u5305", "\u6267\u884c", "\u8fd0\u884c", "\u542f\u52a8", "\u5229\u7528", "\u76f4\u63a5\u505a", "\u52a8\u624b", "\u76f4\u63a5\u5904\u7406", "\u5f00\u59cb\u5e72", "\u5f00\u59cb\u5de5\u4f5c")),
    ("validate", ("validate", "verify", "reproduce", "test", "\u9a8c\u8bc1", "\u786e\u8ba4", "\u590d\u73b0", "\u6d4b\u8bd5")),
    ("impact", ("prove impact", "measure impact", "impact", "\u8bc1\u660e\u5f71\u54cd", "\u5f71\u54cd\u8bc4\u4f30", "\u5f71\u54cd")),
    ("coverage", ("coverage", "negative control", "false positive", "\u8986\u76d6", "\u8d1f\u5411\u5bf9\u7167", "\u8bef\u62a5")),
    ("repair", ("fix", "repair", "\u4fee\u590d", "\u52a0\u56fa")),
    ("package", ("package", "release", "publish", "\u6253\u5305", "\u53d1\u5e03", "\u4e0a\u4f20")),
    ("rollback", ("rollback", "restore", "cleanup", "\u56de\u6eda", "\u6062\u590d", "\u6e05\u7406", "\u5378\u8f7d")),
    ("report", ("report", "summarize", "document", "\u62a5\u544a", "\u603b\u7ed3", "\u8bf4\u660e", "\u8bb0\u5f55")),
)

_ACTION_ARTIFACTS: dict[str, tuple[str, ...]] = {
    "acquire": ("surface_map",),
    "inspect": ("surface_map",),
    "discover": ("surface_map",),
    "transform": ("reproduction_artifact",),
    "implement": ("reproduction_artifact",),
    "execute": ("reproduction_artifact",),
    "validate": ("reproduction_artifact",),
    "repair": ("reproduction_artifact",),
    "package": ("reproduction_artifact",),
    "impact": ("impact_proof",),
    "coverage": ("coverage_report",),
    "rollback": ("cleanup_proof",),
    "report": ("final_report",),
}

_DELIVERABLE_ARTIFACTS: dict[str, tuple[str, ...]] = {
    "plan": ("hypothesis_queue",),
    "code": ("reproduction_artifact",),
    "artifact": ("reproduction_artifact",),
    "evidence": ("reproduction_artifact",),
    "tests": ("reproduction_artifact",),
    "report": ("final_report",),
}

_ARTIFACT_ORDER = (
    "surface_map",
    "hypothesis_queue",
    "reproduction_artifact",
    "impact_proof",
    "coverage_report",
    "cleanup_proof",
    "final_report",
)

_DELIVERABLES: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("plan", ("plan", "proposal", "\u65b9\u6848", "\u8ba1\u5212")),
    ("code", ("code", "script", "patch", "poc", "\u4ee3\u7801", "\u811a\u672c", "\u8865\u4e01")),
    ("artifact", ("artifact", "file", "binary", "archive", "sample", "\u4ea7\u7269", "\u6587\u4ef6", "\u7a0b\u5e8f", "\u538b\u7f29\u5305", "\u6837\u672c", "\u526f\u672c")),
    ("evidence", ("evidence", "request", "response", "log", "\u8bc1\u636e", "\u8bf7\u6c42", "\u54cd\u5e94", "\u65e5\u5fd7")),
    ("report", ("report", "release notes", "\u62a5\u544a", "\u53d1\u5e03\u8bf4\u660e", "\u603b\u7ed3")),
    ("tests", ("test", "validate", "verify", "regression", "benchmark", "\u6d4b\u8bd5", "\u9a8c\u8bc1", "\u56de\u5f52", "\u57fa\u51c6")),
)

_CONSTRAINT_MARKERS = (
    "must", "do not", "don't", "only", "without", "keep", "preserve",
    "\u5fc5\u987b", "\u4e0d\u8981", "\u4e0d\u5f97", "\u53ea\u80fd", "\u4ec5", "\u4e0d\u4f9d\u8d56", "\u4fdd\u7559", "\u4e0d\u80fd",
)
_PLAN_ONLY_MARKERS = (
    "plan only", "proposal only", "only give me a plan", "give me a plan first",
    "give me a plan", "provide a plan first", "plan first", "no changes yet",
    "do not make changes yet", "don't make changes yet", "don't change anything yet",
    "\u53ea\u7ed9\u65b9\u6848", "\u53ea\u8981\u65b9\u6848", "\u5148\u7ed9\u65b9\u6848", "\u5148\u7ed9\u6211\u65b9\u6848",
    "\u7ed9\u6211\u65b9\u6848\u5373\u53ef", "\u65b9\u6848\u5373\u53ef", "\u6682\u4e0d\u4fee\u6539", "\u6682\u4e0d\u6539\u52a8",
    "\u5148\u4e0d\u8981\u4fee\u6539", "\u5148\u522b\u6539", "\u5148\u4e0d\u8981\u52a8",
)
_EXECUTION_MARKERS = tuple(marker for name, markers in _VERBS if name != "report" for marker in markers)
_NEGATION_MARKERS = (
    "do not", "don't", "without", "need not", "no need to", "never", "avoid",
    "\u4e0d\u8981", "\u4e0d\u5f97", "\u4e0d\u9700", "\u65e0\u9700", "\u4e0d\u7528", "\u4e0d\u5fc5", "\u522b",
    "\u5207\u52ff", "\u7981\u6b62", "\u6682\u4e0d", "\u5148\u4e0d", "\u5148\u522b",
)


def _marker_expression(marker: str) -> str:
    needle = marker.casefold()
    escaped = re.escape(needle)
    if any("\u4e00" <= character <= "\u9fff" for character in needle) or " " in needle or "-" in needle:
        return escaped
    return rf"(?<![a-z0-9_]){escaped}(?![a-z0-9_])"


class _MarkerIndex:
    """Precompiled marker index used for one scan per source or clause."""

    def __init__(self, table: Sequence[tuple[str, Sequence[str]]]) -> None:
        names_by_marker: dict[str, list[str]] = {}
        self.order = tuple(name for name, _ in table)
        for name, markers in table:
            for marker in markers:
                names_by_marker.setdefault(marker.casefold(), []).append(name)
        ordered_markers = sorted(names_by_marker, key=lambda marker: (-len(marker), marker))
        self.names_by_marker = {marker: tuple(names) for marker, names in names_by_marker.items()}
        self.pattern = re.compile("|".join(_marker_expression(marker) for marker in ordered_markers))

    def classify(self, text: str, *, requested: bool = False) -> tuple[str, ...]:
        folded = text.casefold()
        found: set[str] = set()
        for match in self.pattern.finditer(folded):
            if requested and _execution_occurrence_is_negated(folded, match.start(), folded=True):
                continue
            found.update(self.names_by_marker[match.group(0)])
        return tuple(name for name in self.order if name in found)

    def has_requested(self, text: str) -> bool:
        folded = text.casefold()
        return any(
            not _execution_occurrence_is_negated(folded, match.start(), folded=True)
            for match in self.pattern.finditer(folded)
        )


class _LiteralIndex:
    """Aho-Corasick index for target binding without clauses×targets scans."""

    def __init__(self, values: Sequence[str]) -> None:
        self.values = tuple(values)
        self.lengths = tuple(len(value) for value in self.values)
        self.transitions: list[dict[str, int]] = [{}]
        self.failures: list[int] = [0]
        self.outputs: list[list[int]] = [[]]
        for value_index, value in enumerate(values):
            state = 0
            for character in value:
                next_state = self.transitions[state].get(character)
                if next_state is None:
                    next_state = len(self.transitions)
                    self.transitions[state][character] = next_state
                    self.transitions.append({})
                    self.failures.append(0)
                    self.outputs.append([])
                state = next_state
            self.outputs[state].append(value_index)
        pending: deque[int] = deque(self.transitions[0].values())
        while pending:
            state = pending.popleft()
            for character, next_state in self.transitions[state].items():
                pending.append(next_state)
                failure = self.failures[state]
                while failure and character not in self.transitions[failure]:
                    failure = self.failures[failure]
                self.failures[next_state] = self.transitions[failure].get(character, 0)
                self.outputs[next_state].extend(self.outputs[self.failures[next_state]])

    def matches(self, text: str) -> set[int]:
        return {value_index for _, _, value_index in self.spans(text)}

    def spans(self, text: str) -> tuple[tuple[int, int, int], ...]:
        """Return every literal span as ``(start, end, value_index)``."""

        state = 0
        found: list[tuple[int, int, int]] = []
        for position, character in enumerate(text):
            while state and character not in self.transitions[state]:
                state = self.failures[state]
            state = self.transitions[state].get(character, 0)
            for value_index in self.outputs[state]:
                end = position + 1
                found.append((end - self.lengths[value_index], end, value_index))
        return tuple(found)


def _mask_explicit_targets(text: str, targets: Sequence[str]) -> str:
    """Hide exact target literals from intent classification without loss.

    Concrete paths, URLs and identifiers may themselves contain words such as
    ``rewrite``, ``test`` or ``report``.  Those bytes identify the object of the
    request; they are not user-requested actions.  The authoritative source is
    retained verbatim while this same-length projection is used only for
    syntax classification and clause-boundary discovery.
    """

    literals = tuple(dict.fromkeys(str(target) for target in targets if str(target)))
    if not literals:
        return text
    masked = bytearray(len(text))
    for start, end, _ in _LiteralIndex(literals).spans(text):
        if start >= 0:
            masked[start:end] = b"\x01" * (end - start)
    return "".join(" " if masked[index] else character for index, character in enumerate(text))


_SCENE_INDEX = _MarkerIndex(_SCENE_MARKERS)
_VERB_INDEX = _MarkerIndex(_VERBS)
_DELIVERABLE_INDEX = _MarkerIndex(_DELIVERABLES)
_CONSTRAINT_INDEX = _MarkerIndex((("constraint", _CONSTRAINT_MARKERS),))
_PLAN_ONLY_INDEX = _MarkerIndex((("plan", _PLAN_ONLY_MARKERS),))
_NEGATED_ENGLISH = re.compile(
    r"(?:do\s+not|don't|without|not\s+to|need\s+not|no\s+need\s+to|never|avoid)"
    r"(?:\s+[a-z0-9_+-]+){0,8}\s*$"
)
_NEGATED_CHINESE = re.compile(
    r"(?:\u4e0d\u8981|\u4e0d\u5f97|\u4e0d\u9700|\u65e0\u9700|\u4e0d\u7528|\u4e0d\u5fc5|\u522b|\u5207\u52ff|\u7981\u6b62|"
    r"\u6682\u4e0d|\u5148\u4e0d|\u5148\u522b|\u4e0d\u518d|\u4e0d\u8fdb\u884c)[^\uff0c,\uff1b;\u3002.!?]{0,8}$"
)


def _contains(text: str, marker: str) -> bool:
    lowered = text.casefold()
    needle = marker.casefold()
    if any("\u4e00" <= character <= "\u9fff" for character in needle) or " " in needle or "-" in needle:
        return needle in lowered
    return bool(re.search(rf"(?<![a-z0-9_]){re.escape(needle)}(?![a-z0-9_])", lowered))


def _classify(text: str, table: Sequence[tuple[str, Sequence[str]]]) -> tuple[str, ...]:
    if table is _SCENE_MARKERS:
        return _SCENE_INDEX.classify(text)
    return _MarkerIndex(table).classify(text)


def split_clauses(text: str, *, protected_literals: Sequence[str] = ()) -> tuple[str, ...]:
    """Return every non-empty clause; never truncate user input."""

    projection = _mask_explicit_targets(text, protected_literals)
    clauses: list[str] = []
    offset = 0
    for boundary in _CLAUSE_BOUNDARY.finditer(projection):
        part = text[offset:boundary.start()].strip(" \t,\uFF0C\uFF1B")
        if part:
            clauses.append(part)
        offset = boundary.end()
    part = text[offset:].strip(" \t,\uFF0C\uFF1B")
    if part:
        clauses.append(part)
    return tuple(clauses) or (text,)


def _scene(text: str) -> str:
    scenes = _SCENE_INDEX.classify(text)
    return scenes[0] if scenes else "general"


def _constraint_clauses(
    clauses: Sequence[str],
    *,
    analysis_clauses: Sequence[str] | None = None,
) -> tuple[str, ...]:
    projections = clauses if analysis_clauses is None else analysis_clauses
    return tuple(
        clause
        for clause, projection in zip(clauses, projections)
        if _CONSTRAINT_INDEX.pattern.search(projection.casefold())
    )


def _clause_id(index: int, clause: str) -> str:
    digest = hashlib.sha256(clause.encode("utf-8")).hexdigest()[:12]
    return f"clause-{index + 1:02d}-{digest}"


def _marker_positions(text: str, marker: str) -> tuple[int, ...]:
    lowered = text.casefold()
    needle = marker.casefold()
    if any("\u4e00" <= character <= "\u9fff" for character in needle) or " " in needle or "-" in needle:
        positions: list[int] = []
        offset = 0
        while True:
            index = lowered.find(needle, offset)
            if index < 0:
                return tuple(positions)
            positions.append(index)
            offset = index + max(1, len(needle))
    return tuple(match.start() for match in re.finditer(rf"(?<![a-z0-9_]){re.escape(needle)}(?![a-z0-9_])", lowered))


def _execution_occurrence_is_negated(text: str, position: int, *, folded: bool = False) -> bool:
    normalized = text if folded else text.casefold()
    prefix = normalized[max(0, position - 96):position].rstrip()
    if any(prefix.endswith(marker.casefold()) for marker in _NEGATION_MARKERS):
        return True
    return bool(_NEGATED_ENGLISH.search(prefix) or _NEGATED_CHINESE.search(prefix))


def _classify_requested(text: str, table: Sequence[tuple[str, Sequence[str]]]) -> tuple[str, ...]:
    """Classify requested work while leaving negated words as constraints."""

    if table is _VERBS:
        return _VERB_INDEX.classify(text, requested=True)
    if table is _DELIVERABLES:
        return _DELIVERABLE_INDEX.classify(text, requested=True)
    return _MarkerIndex(table).classify(text, requested=True)


def _has_positive_execution(verbs: Sequence[str]) -> bool:
    return any(verb != "report" for verb in verbs)


def _has_affirmative_plan_only_marker(text: str) -> bool:
    return _PLAN_ONLY_INDEX.has_requested(text)


def _clause_contract(
    clause_id: str,
    clause: str,
    *,
    targets: Sequence[str],
    execution_required: bool,
) -> dict[str, object]:
    analysis_clause = _mask_explicit_targets(clause, targets)
    actions = _classify_requested(analysis_clause, _VERBS)
    deliverables = _classify_requested(analysis_clause, _DELIVERABLES)
    required = {
        artifact
        for name in (*actions, *deliverables)
        for artifact in (*_ACTION_ARTIFACTS.get(name, ()), *_DELIVERABLE_ARTIFACTS.get(name, ()))
    }
    if not required:
        required.add("reproduction_artifact" if execution_required else "hypothesis_queue")
    return {
        "clause_id": clause_id,
        "source_text": clause,
        "actions": list(actions),
        "deliverables": list(deliverables),
        "constraints": list(_constraint_clauses((clause,), analysis_clauses=(analysis_clause,))),
        "targets": list(targets),
        "required_artifacts": [artifact for artifact in _ARTIFACT_ORDER if artifact in required],
    }


@dataclass(frozen=True, repr=False)
class PromptRewrite:
    version: str
    source_text: str
    source_sha256: str
    source_bytes: int
    action_kind: str
    execution_required: bool
    scene: str
    clauses: tuple[str, ...]
    clause_ids: tuple[str, ...]
    clause_contracts: tuple[dict[str, object], ...]
    verbs: tuple[str, ...]
    actions: tuple[str, ...]
    targets: tuple[str, ...]
    deliverables: tuple[str, ...]
    constraints: tuple[str, ...]
    unresolved_clauses: tuple[str, ...]
    execution_prompt: str
    lossless: bool = True
    authoritative_source: str = "goal.objective"
    source_representation: str = "original-source"
    original_source_sha256: str = ""
    original_source_bytes: int = 0

    def __repr__(self) -> str:
        return (
            "PromptRewrite("
            f"version={self.version!r}, source_sha256={self.source_sha256!r}, "
            f"source_bytes={self.source_bytes}, clause_count={len(self.clauses)}, "
            f"lossless={self.lossless}, source_representation={self.source_representation!r})"
        )

    def _raw_payload(self) -> dict[str, object]:
        return {item.name: getattr(self, item.name) for item in fields(self)}

    def to_dict(self) -> dict[str, object]:
        return json.loads(json.dumps(self._raw_payload(), ensure_ascii=False))

    @property
    def fingerprint(self) -> str:
        payload = json.dumps(self._raw_payload(), ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        return hashlib.sha256(payload.encode("utf-8")).hexdigest()

    @property
    def clause_ids_sha256(self) -> str:
        payload = json.dumps(self.clause_ids, ensure_ascii=False, separators=(",", ":"))
        return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def rewrite_objective(
    objective: str,
    *,
    targets: Sequence[str] = (),
    action_kind_override: str | None = None,
    execution_required_override: bool | None = None,
    source_representation: str = "original-source",
    original_source_sha256: str = "",
    original_source_bytes: int | None = None,
) -> PromptRewrite:
    if not isinstance(objective, str) or not objective.strip():
        raise ValueError("objective_required")
    if action_kind_override not in {None, "control"}:
        raise ValueError("action_kind_override_invalid")
    if execution_required_override is not None and (
        action_kind_override != "control" or not isinstance(execution_required_override, bool)
    ):
        raise ValueError("execution_required_override_invalid")
    source = objective
    resolved_targets = tuple(dict.fromkeys(str(target) for target in targets if str(target)))
    analysis_source = _mask_explicit_targets(source, resolved_targets)
    clauses = split_clauses(source, protected_literals=resolved_targets)
    verbs = _classify_requested(analysis_source, _VERBS)
    deliverables = _classify_requested(analysis_source, _DELIVERABLES)
    plan_only = _has_affirmative_plan_only_marker(analysis_source) and not _has_positive_execution(verbs)
    action_kind = action_kind_override or ("plan" if plan_only else "execute")
    execution_required = (
        bool(execution_required_override)
        if execution_required_override is not None
        else not plan_only
    )
    clause_ids = tuple(_clause_id(index, clause) for index, clause in enumerate(clauses))
    unresolved = tuple(clause_id for clause_id, clause in zip(clause_ids, clauses) if not _TOKEN.search(clause))
    if len(resolved_targets) <= 8:
        clause_targets = tuple(
            tuple(target for target in resolved_targets if target in clause)
            for clause in clauses
        )
    else:
        target_index = _LiteralIndex(resolved_targets)
        clause_targets = tuple(
            tuple(resolved_targets[index] for index in sorted(target_index.matches(clause)))
            for clause in clauses
        )
    clause_contracts = tuple(
        _clause_contract(
            clause_id,
            clause,
            targets=bound_targets,
            execution_required=execution_required,
        )
        for clause_id, clause, bound_targets in zip(clause_ids, clauses, clause_targets)
    )
    source_encoded = source.encode("utf-8")
    source_sha256 = hashlib.sha256(source_encoded).hexdigest()
    source_bytes = len(source_encoded)
    if source_representation not in {"original-source", "secret-reference-v1"}:
        raise ValueError("source_representation_invalid")
    original_sha256 = original_source_sha256 or source_sha256
    original_bytes = source_bytes if original_source_bytes is None else original_source_bytes
    if re.fullmatch(r"[0-9a-f]{64}", original_sha256) is None:
        raise ValueError("original_source_sha256_invalid")
    if not isinstance(original_bytes, int) or isinstance(original_bytes, bool) or original_bytes < 0:
        raise ValueError("original_source_bytes_invalid")
    lossless = (
        source_representation == "original-source"
        and original_sha256 == source_sha256
        and original_bytes == source_bytes
    )
    source_heading = (
        "Original objective (verbatim):"
        if lossless
        else "Durable objective projection (Secret References; not verbatim original):"
    )
    authority = (
        "Authority: execute source_text exactly; this rewrite augments it and never replaces it."
        if lossless
        else "Authority: the ephemeral original remains authoritative; this durable projection preserves structure "
        "and stable Secret References but is not the verbatim original."
    )
    scene = _scene(analysis_source)
    analysis_clauses = tuple(_mask_explicit_targets(clause, bound) for clause, bound in zip(clauses, clause_targets))
    constraints = _constraint_clauses(clauses, analysis_clauses=analysis_clauses)
    clause_ids_payload = json.dumps(clause_ids, ensure_ascii=False, separators=(",", ":"))
    clause_ids_sha256 = hashlib.sha256(clause_ids_payload.encode("utf-8")).hexdigest()
    required_artifacts = tuple(
        artifact
        for artifact in _ARTIFACT_ORDER
        if any(artifact in contract["required_artifacts"] for contract in clause_contracts)
    )
    lines = (
        f"[prompt-rewrite:{REWRITE_VERSION}]",
        authority,
        f"Source projection: sha256={source_sha256} bytes={source_bytes} representation={source_representation}",
        f"Original source identity: sha256={original_sha256} bytes={original_bytes}",
        f"Mode: {action_kind}; execution_required={str(execution_required).lower()}; scene={scene}",
        f"Clause index: count={len(clause_ids)} ids_sha256={clause_ids_sha256}",
        f"Actions: {','.join(verbs) or 'unspecified'}",
        f"Deliverables: {','.join(deliverables) or 'unspecified'}",
        f"Constraints: count={len(constraints)}",
        f"Required artifacts: {','.join(required_artifacts)}",
        source_heading,
        source,
        "Clause contracts: use the structured clause_contracts field bound by the Clause index digest above.",
        "Preserve: every concrete identifier, target, constraint, requested format, action, and deliverable.",
        "Completion: each clause must have target-bound evidence or an explicit durable unresolved dependency.",
    )
    return PromptRewrite(
        version=REWRITE_VERSION,
        source_text=source,
        source_sha256=source_sha256,
        source_bytes=source_bytes,
        action_kind=action_kind,
        execution_required=execution_required,
        scene=scene,
        clauses=clauses,
        clause_ids=clause_ids,
        clause_contracts=clause_contracts,
        verbs=verbs,
        actions=verbs,
        targets=resolved_targets,
        deliverables=deliverables,
        constraints=constraints,
        unresolved_clauses=unresolved,
        execution_prompt="\n".join(lines),
        lossless=lossless,
        authoritative_source=("goal.objective" if lossless else "ephemeral-original"),
        source_representation=source_representation,
        original_source_sha256=original_sha256,
        original_source_bytes=original_bytes,
    )
