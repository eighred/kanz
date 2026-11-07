#!/usr/bin/env bash
# Verify the reviewed immutable producer before executing its public tools.
# Fresh per-image audit results are never cached or supplied by this artifact.
set -euo pipefail
rustup toolchain install 1.90.0 --profile minimal
python3 -B tools/rust-audit-tools/consume.py
