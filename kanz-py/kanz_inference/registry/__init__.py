"""Model versioning + artifact registry (PRED-09).

Provides the lookup-by-id and lookup-by-feature-set surface both
inference paths (PRED-04 streaming, PRED-08 interactive) call when
they need a Model to score against. Also the registration point for
PRED-10's shadow / canary path (primary vs shadow distinction).

Importable names:

- ``ModelMetadata`` — per-model contract: id, feature_set_ref it
  consumes, confidence threshold below which predictions are
  DEGRADED per PRED-02 §2.3, artifact_uri for traceability.
- ``Registry`` — in-memory mapping with register / get_by_id /
  primary_for_feature_set / shadows_for_feature_set / list_models.
- ``ModelLoader`` — Protocol for the optional pluggable artifact-
  load step (filesystem, S3, registry server, ...). PRED-09 ships
  the registry data structure; how artifacts become Model
  instances is a deployment concern that can plug in via this
  protocol.
"""

from kanz_inference.registry.registry import (
    ModelLoader,
    ModelMetadata,
    Registry,
)

__all__ = ["ModelLoader", "ModelMetadata", "Registry"]
