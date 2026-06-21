"""MLOPS-01g model card + approval workflow + lineage tests."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from kanz_inference.registry import ValidationRecord
from kanz_inference.governance import (
    ApprovalError,
    ApprovalStatus,
    ApprovalWorkflow,
    LineageRecord,
    ModelCard,
)


def _valid(model_id: str, *, passed: bool = True, expires_in=timedelta(days=365)) -> ValidationRecord:
    now = datetime.now(timezone.utc)
    return ValidationRecord(
        model_id=model_id, validated_at=now, expires_at=now + expires_in, passed=passed
    )


def _card(model_id: str = "vol-forecast@1.4.2", *, validation=None) -> ModelCard:
    return ModelCard(
        model_id=model_id,
        owner="quant-ml",
        intended_use="1-day vol forecast for equity risk",
        lineage=LineageRecord(
            model_id=model_id,
            feature_set_ref="equity-momentum:7",
            features=("rsi_14", "macd"),
            training_data_refs=("warehouse.features@2026-01-01",),
        ),
        limitations="not calibrated for illiquid names",
        validation=validation if validation is not None else _valid(model_id),
    )


# --- Lineage / card content -------------------------------------------


def test_lineage_must_match_card_model_id():
    wf = ApprovalWorkflow()
    bad = ModelCard(
        model_id="a@1",
        owner="x",
        intended_use="y",
        lineage=LineageRecord(model_id="b@2", feature_set_ref="fs:1"),
    )
    with pytest.raises(ValueError, match="model_id"):
        wf.register_card(bad)


def test_register_card_starts_in_draft():
    wf = ApprovalWorkflow()
    wf.register_card(_card())
    assert wf.status("vol-forecast@1.4.2") == ApprovalStatus.DRAFT
    card = wf.card("vol-forecast@1.4.2")
    assert card.lineage.features == ("rsi_14", "macd")
    assert card.lineage.training_data_refs == ("warehouse.features@2026-01-01",)


# --- Happy-path workflow ----------------------------------------------


def test_full_approval_flow_records_approver_and_time():
    now = datetime(2026, 6, 21, 12, 0, 0, tzinfo=timezone.utc)
    wf = ApprovalWorkflow(clock=lambda: now)
    wf.register_card(_card())
    wf.submit("vol-forecast@1.4.2")
    assert wf.status("vol-forecast@1.4.2") == ApprovalStatus.PENDING_REVIEW
    wf.approve("vol-forecast@1.4.2", approver="risk-officer@eighred")

    assert wf.is_approved("vol-forecast@1.4.2")
    state = wf.state("vol-forecast@1.4.2")
    assert state.approver == "risk-officer@eighred"
    assert state.decided_at == now


def test_rejection_records_reason():
    wf = ApprovalWorkflow()
    wf.register_card(_card())
    wf.submit("vol-forecast@1.4.2")
    wf.reject("vol-forecast@1.4.2", approver="risk-officer", reason="insufficient backtest window")
    state = wf.state("vol-forecast@1.4.2")
    assert state.status == ApprovalStatus.REJECTED
    assert state.reason == "insufficient backtest window"
    assert not wf.is_approved("vol-forecast@1.4.2")


def test_rejected_card_can_be_resubmitted():
    wf = ApprovalWorkflow()
    wf.register_card(_card())
    wf.submit("vol-forecast@1.4.2")
    wf.reject("vol-forecast@1.4.2", "officer", "fix it")
    wf.submit("vol-forecast@1.4.2")  # resubmit after fixes
    assert wf.status("vol-forecast@1.4.2") == ApprovalStatus.PENDING_REVIEW


# --- Approval gate ----------------------------------------------------


def test_approve_requires_passing_validation():
    wf = ApprovalWorkflow()
    wf.register_card(_card(validation=_valid("vol-forecast@1.4.2", passed=False)))
    wf.submit("vol-forecast@1.4.2")
    with pytest.raises(ApprovalError, match="validation"):
        wf.approve("vol-forecast@1.4.2", "officer")


def test_approve_requires_non_expired_validation():
    wf = ApprovalWorkflow()
    wf.register_card(_card(validation=_valid("vol-forecast@1.4.2", expires_in=timedelta(seconds=-1))))
    wf.submit("vol-forecast@1.4.2")
    with pytest.raises(ApprovalError, match="validation"):
        wf.approve("vol-forecast@1.4.2", "officer")


def test_approve_requires_validation_present():
    wf = ApprovalWorkflow()
    card = ModelCard(  # a card carrying no validation evidence at all
        model_id="x@1", owner="o", intended_use="u",
        lineage=LineageRecord(model_id="x@1", feature_set_ref="fs:1"),
        validation=None,
    )
    wf.register_card(card)
    wf.submit("x@1")
    with pytest.raises(ApprovalError):
        wf.approve("x@1", "officer")


# --- State-machine guards ---------------------------------------------


def test_cannot_approve_without_submitting():
    wf = ApprovalWorkflow()
    wf.register_card(_card())
    with pytest.raises(ApprovalError, match="submit"):
        wf.approve("vol-forecast@1.4.2", "officer")


def test_cannot_submit_unknown_card():
    wf = ApprovalWorkflow()
    with pytest.raises(ApprovalError, match="no card"):
        wf.submit("ghost@1")


def test_cannot_overwrite_approved_card():
    wf = ApprovalWorkflow()
    wf.register_card(_card())
    wf.submit("vol-forecast@1.4.2")
    wf.approve("vol-forecast@1.4.2", "officer")
    with pytest.raises(ApprovalError, match="approved"):
        wf.register_card(_card())  # a revision must not silently discard sign-off


def test_approver_required():
    wf = ApprovalWorkflow()
    wf.register_card(_card())
    wf.submit("vol-forecast@1.4.2")
    with pytest.raises(ValueError, match="approver"):
        wf.approve("vol-forecast@1.4.2", "")


def test_status_of_unknown_is_none():
    assert ApprovalWorkflow().status("nope@1") is None
