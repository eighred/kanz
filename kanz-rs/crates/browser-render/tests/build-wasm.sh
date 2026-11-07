#!/usr/bin/env bash
set -euo pipefail
# Qualification recipe only. Toolchain and target must already be installed;
# this script neither installs tools nor changes the production browser build.
cd "$(dirname "$0")/../../.."
cargo +1.90.0 rustc --locked --release -p kanz-browser-render --target wasm32-unknown-unknown -- \
  -C link-arg=--max-memory=2097152 -C link-arg=-zstack-size=65536
node --experimental-strip-types crates/browser-render/tests/wasm.mjs \
  target/wasm32-unknown-unknown/release/kanz_browser_render.wasm
