#!/usr/bin/env bash
set -euo pipefail
# Qualification delegates the same production asset compilation recipe.
# Toolchain and target must already be installed; neither script installs tools.
cd "$(dirname "$0")/../../.."
node ../kanz-web/scripts/build-renderer.mjs
node --experimental-strip-types crates/browser-render/tests/wasm.mjs \
  target/wasm32-unknown-unknown/release/kanz_browser_render.wasm
