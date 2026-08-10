from __future__ import annotations

import hashlib
import math
import re
from pathlib import Path
from typing import Any, Mapping, Sequence

from .models import GoalContract, GoalCriterion, SuccessPredicate
from .intent_rewriter import rewrite_objective
from .security import SECRET_REFERENCE_RE, canonicalize_sensitive_text, find_secret_references, project_sensitive


URL_RE = re.compile(r"\bhttps?://[^\s<>'\"]+", re.IGNORECASE)
HOST_RE = re.compile(
    r"(?<![\w.-])(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:[a-z]{2,63}|invalid|test)(?::\d{1,5})?(?![\w.-])",
    re.IGNORECASE,
)
IP_RE = re.compile(r"(?<![\d.])(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?(?![\d.])")
PATH_RE = re.compile(r"(?<!\w)(?:[A-Za-z]:[\\/]|\.?\.?[\\/])[^\s<>'\"]+")
TOKEN_RE = re.compile(r"[a-z0-9][a-z0-9_.+-]*|[\u4e00-\u9fff]+", re.IGNORECASE)
SUPPORTED_SUCCESS_PREDICATE_OPERATORS = frozenset(
    {"exists", "eq", "ne", "gte", "gt", "lte", "lt", "contains"}
)
SUPPORTED_SUCCESS_PREDICATE_KINDS = frozenset(
    {"workflow_actions_complete", "artifact_verified", "artifact_count", "artifact_field"}
)
RUNTIME_ARTIFACT_TYPES = frozenset(
    {
        "surface_map",
        "hypothesis_queue",
        "reproduction_artifact",
        "impact_proof",
        "coverage_report",
        "cleanup_proof",
        "final_report",
    }
)
PLAN_ONLY_ARTIFACT_TYPES = frozenset({"surface_map", "hypothesis_queue", "final_report"})
CLAUSE_SPLIT_RE = re.compile(r"\s+(?:and|then|plus)\s+|[;；。]|(?:以及|并且|然后|和)", re.IGNORECASE)
TARGET_TRAILING_PUNCTUATION = ".,;:!?)]}，。；：！？）】"


def _trim_target(value: str) -> str:
    target = value
    while target and target[-1] in TARGET_TRAILING_PUNCTUATION:
        if target.endswith("]") and any(
            match.end() == len(target) for match in SECRET_REFERENCE_RE.finditer(target)
        ):
            break
        target = target[:-1]
    return target


def _matches_marker(normalized: str, tokens: set[str], marker: str) -> bool:
    return marker in normalized if any("\u4e00" <= character <= "\u9fff" for character in marker) else marker in tokens


WORKFLOW_MARKERS: tuple[tuple[str, frozenset[str]], ...] = (
    (
        "model-security-assessment",
        frozenset({"jailbreak", "prompt-injection", "prompt", "llm", "模型", "越狱", "提示词", "注入"}),
    ),
    (
        "binary-mobile-analysis",
        frozenset({"apk", "android", "ios", "binary", "firmware", "ida", "jadx", "逆向", "二进制", "固件", "移动端"}),
    ),
    (
        "source-assisted-review",
        frozenset({"source", "repository", "codebase", "code-audit", "source-code", "源码", "代码审计", "仓库"}),
    ),
    (
        "identity-cloud-operation",
        frozenset({"active-directory", "kerberos", "entra", "azure", "aws", "gcp", "iam", "ad", "域", "云", "身份"}),
    ),
    (
        "adversary-emulation",
        frozenset({"emulation", "adversary-emulation", "atomic", "ttp", "purple-team", "对抗模拟", "攻击链", "紫队"}),
    ),
    (
        "web-api-assessment",
        frozenset({"web", "api", "http", "https", "graphql", "sqli", "xss", "ssrf", "xxe", "ssti", "网页", "接口", "网站"}),
    ),
    (
        "external-assessment",
        frozenset({"recon", "domain", "host", "port", "network", "scan", "侦察", "域名", "端口", "网络", "扫描"}),
    ),
)


class GoalCompiler:
    @staticmethod
    def _validate_success_predicate(predicate: SuccessPredicate) -> None:
        """Reject terminal contracts that the Runtime cannot evaluate."""

        if predicate.kind not in SUPPORTED_SUCCESS_PREDICATE_KINDS:
            raise ValueError(f"success_predicate_kind_unsupported:{predicate.kind}")
        if predicate.operator not in SUPPORTED_SUCCESS_PREDICATE_OPERATORS:
            raise ValueError(f"success_predicate_operator_unsupported:{predicate.operator}")
        if predicate.kind in {"workflow_actions_complete", "artifact_verified"}:
            if predicate.operator != "exists":
                raise ValueError(
                    f"success_predicate_operator_invalid_for_kind:{predicate.kind}:{predicate.operator}"
                )
        if predicate.kind == "workflow_actions_complete" and predicate.subject not in {"", "required"}:
            raise ValueError("success_predicate_subject_invalid:workflow_actions_complete")
        if predicate.kind != "workflow_actions_complete" and not predicate.subject.strip():
            raise ValueError(f"success_predicate_subject_required:{predicate.kind}")
        if predicate.kind == "artifact_count":
            if predicate.operator not in {"eq", "ne", "gte", "gt", "lte", "lt"}:
                raise ValueError(
                    f"success_predicate_operator_invalid_for_kind:{predicate.kind}:{predicate.operator}"
                )
            if isinstance(predicate.value, bool) or not isinstance(predicate.value, (int, float)):
                raise ValueError("success_predicate_value_invalid:artifact_count")
            if isinstance(predicate.value, float) and not math.isfinite(predicate.value):
                raise ValueError("success_predicate_value_invalid:artifact_count")
        if predicate.kind == "artifact_field":
            artifact_type, separator, field_name = predicate.subject.partition(".")
            if not separator or not artifact_type.strip() or not field_name.strip():
                raise ValueError("success_predicate_subject_invalid:artifact_field")
        if predicate.kind in {"artifact_verified", "artifact_count", "artifact_field"}:
            artifact_type = predicate.subject.partition(".")[0]
            if artifact_type not in RUNTIME_ARTIFACT_TYPES:
                raise ValueError(f"success_predicate_artifact_unsupported:{artifact_type}")

    def extract_targets(self, objective: str) -> tuple[str, ...]:
        discovered: list[tuple[int, int, int, str]] = []
        for priority, pattern in enumerate((URL_RE, IP_RE, HOST_RE, PATH_RE)):
            for match in pattern.finditer(objective):
                target = _trim_target(match.group(0))
                if target:
                    discovered.append((match.start(), match.start() + len(target), priority, target))
        discovered.sort(key=lambda item: (item[0], item[2], -(item[1] - item[0])))
        candidates: list[str] = []
        occupied: list[tuple[int, int]] = []
        for start, end, _, target in discovered:
            if any(start < occupied_end and end > occupied_start for occupied_start, occupied_end in occupied):
                continue
            if target not in candidates:
                candidates.append(target)
                occupied.append((start, end))
        return tuple(candidates)

    def extract_context_targets(self, context: Mapping[str, Any] | None) -> tuple[str, ...]:
        candidates: list[str] = []
        target_keys = {"target", "targets", "url", "uri", "host", "path", "repository", "repo", "binary", "sample"}

        def visit(value: Any, key: str = "") -> None:
            if isinstance(value, Mapping):
                for item_key, item_value in value.items():
                    visit(item_value, str(item_key).casefold())
                return
            if isinstance(value, (list, tuple, set)):
                for item in value:
                    visit(item, key)
                return
            if not isinstance(value, str) or not value.strip():
                return
            cleaned = value.strip()
            extracted = self.extract_targets(cleaned)
            if key in target_keys and not extracted:
                extracted = (cleaned,)
            for target in extracted:
                if target not in candidates:
                    candidates.append(target)

        visit(context or {})
        return tuple(candidates)

    def workflow_hints(self, objective: str, targets: Sequence[str] = ()) -> tuple[str, ...]:
        normalized = objective.casefold().replace("_", "-")
        tokens = set(TOKEN_RE.findall(normalized))
        hints: list[str] = []
        for workflow_id, markers in WORKFLOW_MARKERS:
            if any(_matches_marker(normalized, tokens, marker) for marker in markers):
                hints.append(workflow_id)
        for target in targets:
            suffix = Path(target).suffix.casefold()
            if suffix in {".apk", ".ipa", ".exe", ".dll", ".so", ".bin", ".elf", ".dylib"}:
                hints.append("binary-mobile-analysis")
            if target.casefold().startswith(("http://", "https://")):
                hints.append("web-api-assessment")
        return tuple(dict.fromkeys(hints))

    def workflow_hint(self, objective: str, targets: Sequence[str] = ()) -> str:
        del objective, targets
        return "generic-adaptive"

    def workflow_hints_for_target(self, objective: str, target: str) -> tuple[str, ...]:
        segments = [
            item.strip()
            for item in CLAUSE_SPLIT_RE.split(objective)
            if item.strip()
        ]
        local_context = next((item for item in segments if target in item), objective)
        hints = list(self.workflow_hints(local_context, (target,)))
        normalized = target.casefold()
        suffix = Path(target).suffix.casefold()
        binary_suffixes = {".apk", ".ipa", ".exe", ".dll", ".so", ".bin", ".elf", ".dylib"}
        is_url = normalized.startswith(("http://", "https://"))
        is_local = not is_url and (bool(PATH_RE.search(target)) or Path(target).is_absolute())
        if is_url and suffix not in binary_suffixes:
            hints = [item for item in hints if item not in {"source-assisted-review", "binary-mobile-analysis"}]
        elif is_local and suffix not in binary_suffixes:
            hints = [item for item in hints if item not in {"web-api-assessment", "external-assessment"}]
        elif not is_local and not is_url:
            hints = [item for item in hints if item not in {"source-assisted-review", "binary-mobile-analysis"}]
        return tuple(dict.fromkeys(hints))

    @staticmethod
    def success_criteria(
        objective: str,
        targets: Sequence[str],
        clause_ids: Sequence[str] = (),
    ) -> tuple[GoalCriterion, ...]:
        criteria: list[GoalCriterion] = []
        resolved_targets = tuple(targets) or ("",)
        for target in resolved_targets:
            identity = f"{objective}\0{target}\0generic-adaptive"
            criterion_id = f"criterion-{hashlib.sha256(identity.encode('utf-8')).hexdigest()[:16]}"
            scope = f" for {target}" if target else ""
            clause_scope = ", ".join(clause_ids) if clause_ids else "all objective clauses"
            criteria.append(
                GoalCriterion(
                    criterion_id=criterion_id,
                    statement=f"Complete {clause_scope}{scope} through the generic adaptive evidence path: {objective}",
                    target=target,
                    workflow_id="generic-adaptive",
                )
            )
        return tuple(criteria)

    def compile(
        self,
        objective: str,
        *,
        targets: Sequence[str] | None = None,
        workflow_hint: str = "",
        starting_context: Mapping[str, Any] | None = None,
        constraints: Mapping[str, Any] | None = None,
        success_predicates: Sequence[SuccessPredicate | Mapping[str, Any]] = (),
        max_actions: int = 64,
        max_retries_per_action: int = 2,
    ) -> GoalContract:
        if not isinstance(objective, str) or not objective.strip():
            raise ValueError("objective_required")
        source_objective, input_redaction = canonicalize_sensitive_text(objective)
        canonical_context, _ = project_sensitive(dict(starting_context or {}))
        canonical_constraints, _ = project_sensitive(dict(constraints or {}))
        canonical_target_values: list[str] = []
        for item in targets or ():
            projected_target, _ = project_sensitive(str(item))
            canonical_target = str(projected_target).strip()
            if canonical_target and canonical_target not in canonical_target_values:
                canonical_target_values.append(canonical_target)
        canonical_targets = tuple(canonical_target_values)
        resolved_targets = tuple(
            canonical_targets
            or self.extract_targets(source_objective)
            or self.extract_context_targets(canonical_context)
        )
        projected_hint, _ = project_sensitive(workflow_hint)
        explicit_hints = tuple(
            item.strip()
            for item in re.split(r"[,+]", str(projected_hint))
            if item.strip()
        )
        resolved_hints = tuple(item for item in (explicit_hints or self.workflow_hints(source_objective, resolved_targets)) if item != "generic-adaptive")
        resolved_hint = "generic-adaptive"
        rewrite = rewrite_objective(
            source_objective,
            targets=resolved_targets,
            source_representation=str(input_redaction["representation"]),
            original_source_sha256=str(input_redaction["original_sha256"]),
            original_source_bytes=int(input_redaction["original_bytes"]),
        )
        intent_envelope = rewrite.to_dict()
        intent_envelope["source_sha256"] = hashlib.sha256(source_objective.encode("utf-8")).hexdigest()
        intent_envelope["source_bytes"] = len(source_objective.encode("utf-8"))
        intent_envelope["fingerprint"] = rewrite.fingerprint
        intent_envelope["input_redaction"] = input_redaction
        # Distinguish an objective compiled without a target (which may be
        # late-bound later) from a target-bound rewrite whose target metadata
        # was removed after persistence.  The marker augments, but is not part
        # of, the byte-stable PromptRewrite fingerprint.
        intent_envelope["target_binding"] = "compiled" if resolved_targets else "pending"
        predicates: list[SuccessPredicate] = []
        for item in success_predicates:
            if isinstance(item, SuccessPredicate):
                payload: Mapping[str, Any] = {
                    "kind": item.kind,
                    "subject": item.subject,
                    "operator": item.operator,
                    "value": item.value,
                    "description": item.description,
                }
            elif isinstance(item, Mapping):
                payload = item
            else:
                continue
            projected_payload, _ = project_sensitive(dict(payload))
            predicate = SuccessPredicate.from_dict(projected_payload)
            self._validate_success_predicate(predicate)
            if rewrite.action_kind == "plan" and rewrite.execution_required is False:
                artifact_type = predicate.subject.partition(".")[0]
                if artifact_type and artifact_type not in PLAN_ONLY_ARTIFACT_TYPES:
                    raise ValueError(f"success_predicate_artifact_unreachable_in_plan:{artifact_type}")
            predicates.append(predicate)
        credential_refs = find_secret_references(
            {
                "objective": source_objective,
                "targets": resolved_targets,
                "starting_context": canonical_context,
                "constraints": canonical_constraints,
                "success_predicates": [predicate.__dict__ for predicate in predicates],
            }
        )
        intent_envelope["credential_refs"] = list(credential_refs)
        return GoalContract.create(
            objective=source_objective,
            targets=resolved_targets,
            workflow_hint=resolved_hint,
            workflow_hints=resolved_hints,
            starting_context=dict(canonical_context),
            constraints=dict(canonical_constraints),
            success_criteria=self.success_criteria(source_objective, resolved_targets, rewrite.clause_ids),
            success_predicates=tuple(predicates),
            intent_envelope=intent_envelope,
            stop_conditions=("action_budget_exhausted", "explicit_stop_condition", "nonrecoverable_tool_failure"),
            max_actions=max_actions,
            max_retries_per_action=max_retries_per_action,
        )
