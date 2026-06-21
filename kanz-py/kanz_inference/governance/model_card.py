"""Model cards, approval workflow, and model→feature→training-data
lineage (MLOPS-01g).

The model-risk record of *what a model is and who signed off on it*,
completing the MLOPS governance set: MLOPS-01a/b establish that a model
is validated, MLOPS-01c/e keep it honest in production, and this records
the human-accountable documentation and approval trail SR 11-7 / a model
audit demands.

# Three artifacts

- ``LineageRecord`` — model → features → training data. Which feature set
  and feature names the model consumes and which training-data snapshots
  produced it, so a prediction can be traced back to its inputs and a
  model reproduced. The reproducibility spine of the card.
- ``ModelCard`` — the structured document (à la Google "Model Cards"):
  owner, intended use, limitations, the lineage, and the MLOPS-01a
  validation evidence. Immutable content (frozen); a revision is a new
  card.
- ``ApprovalWorkflow`` — the sign-off state machine over cards:
  DRAFT → PENDING_REVIEW → APPROVED | REJECTED. Approval is gated on a
  passing, non-expired validation in the card (no sign-off without
  evidence), records the approver + timestamp, and is the queryable
  governance gate a deployment can require before serving.

# What it does NOT do

- Gate ``Registry.register`` — that is MLOPS-01a's validation gate. Approval
  is an *additional* human sign-off layer; a deployment that wants
  "approved-only serving" checks ``is_approved`` itself. Keeping the two
  gates separate avoids overloading the registry with workflow state.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Callable

from kanz_inference.registry import ValidationRecord


class ApprovalError(Exception):
    """An illegal approval-workflow transition, or an approval attempted
    without the required validation evidence."""


class ApprovalStatus(Enum):
    DRAFT = "draft"
    PENDING_REVIEW = "pending_review"
    APPROVED = "approved"
    REJECTED = "rejected"


@dataclass(frozen=True)
class LineageRecord:
    """model → features → training-data provenance. The reproducibility
    record: given a model, what fed it."""

    model_id: str
    feature_set_ref: str
    features: tuple[str, ...] = ()
    """The feature names the model consumes (within feature_set_ref)."""
    training_data_refs: tuple[str, ...] = ()
    """Identifiers of the training-data snapshots/datasets the model was
    fit on (e.g. a warehouse table@snapshot, an S3 path, a dataset id)."""


@dataclass(frozen=True)
class ModelCard:
    """Structured model documentation. Immutable — a revision is a new
    card (and re-enters the workflow at DRAFT)."""

    model_id: str
    owner: str
    intended_use: str
    lineage: LineageRecord
    limitations: str = ""
    validation: ValidationRecord | None = None
    """The MLOPS-01a validation evidence the approval gate checks."""


@dataclass(frozen=True)
class ApprovalState:
    """The current workflow position of one card."""

    status: ApprovalStatus
    approver: str = ""
    decided_at: datetime | None = None
    reason: str = ""
    """Populated on rejection."""


class ApprovalWorkflow:
    """The card store + sign-off state machine. Process-local, like the
    PRED-09 registry; one per governance process."""

    def __init__(self, *, clock: Callable[[], datetime] | None = None) -> None:
        self._cards: dict[str, ModelCard] = {}
        self._states: dict[str, ApprovalState] = {}
        self._clock = clock or (lambda: datetime.now(timezone.utc))

    def register_card(self, card: ModelCard) -> None:
        """File a card. A model already mid-workflow (PENDING_REVIEW or
        APPROVED) cannot be silently overwritten — revise from DRAFT or a
        REJECTED state, or the prior sign-off would be discarded unseen."""
        if not card.model_id:
            raise ValueError("ModelCard.model_id required")
        if card.lineage.model_id != card.model_id:
            raise ValueError("LineageRecord.model_id must match the card's model_id")
        existing = self._states.get(card.model_id)
        if existing is not None and existing.status in (
            ApprovalStatus.PENDING_REVIEW,
            ApprovalStatus.APPROVED,
        ):
            raise ApprovalError(
                f"{card.model_id} is {existing.status.value}; cannot overwrite an "
                "in-review or approved card"
            )
        self._cards[card.model_id] = card
        self._states[card.model_id] = ApprovalState(ApprovalStatus.DRAFT)

    def submit(self, model_id: str) -> None:
        """DRAFT (or REJECTED, a resubmission) → PENDING_REVIEW."""
        state = self._require_state(model_id)
        if state.status not in (ApprovalStatus.DRAFT, ApprovalStatus.REJECTED):
            raise ApprovalError(
                f"cannot submit {model_id} from {state.status.value}"
            )
        self._states[model_id] = ApprovalState(ApprovalStatus.PENDING_REVIEW)

    def approve(self, model_id: str, approver: str) -> None:
        """PENDING_REVIEW → APPROVED. Gated: the card must carry a passing,
        non-expired MLOPS-01a validation (no sign-off without evidence)."""
        if not approver:
            raise ValueError("approver required")
        state = self._require_state(model_id)
        if state.status != ApprovalStatus.PENDING_REVIEW:
            raise ApprovalError(
                f"cannot approve {model_id} from {state.status.value}; submit it first"
            )
        record = self._cards[model_id].validation
        if record is None or not record.is_valid(self._clock()):
            raise ApprovalError(
                f"cannot approve {model_id}: a recorded, non-expired, passing "
                "validation is required"
            )
        self._states[model_id] = ApprovalState(
            ApprovalStatus.APPROVED, approver=approver, decided_at=self._clock()
        )

    def reject(self, model_id: str, approver: str, reason: str) -> None:
        """PENDING_REVIEW → REJECTED, recording who and why."""
        if not approver:
            raise ValueError("approver required")
        state = self._require_state(model_id)
        if state.status != ApprovalStatus.PENDING_REVIEW:
            raise ApprovalError(
                f"cannot reject {model_id} from {state.status.value}"
            )
        self._states[model_id] = ApprovalState(
            ApprovalStatus.REJECTED, approver=approver, decided_at=self._clock(), reason=reason
        )

    def status(self, model_id: str) -> ApprovalStatus | None:
        state = self._states.get(model_id)
        return state.status if state is not None else None

    def state(self, model_id: str) -> ApprovalState | None:
        return self._states.get(model_id)

    def card(self, model_id: str) -> ModelCard | None:
        return self._cards.get(model_id)

    def is_approved(self, model_id: str) -> bool:
        state = self._states.get(model_id)
        return state is not None and state.status == ApprovalStatus.APPROVED

    def _require_state(self, model_id: str) -> ApprovalState:
        state = self._states.get(model_id)
        if state is None:
            raise ApprovalError(f"no card registered for {model_id}")
        return state
