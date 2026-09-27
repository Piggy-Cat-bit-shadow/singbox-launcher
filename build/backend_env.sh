#!/usr/bin/env bash
# Shared Go build environment for the headless JiejieBox backend.
#
# The headless tag removes the Fyne canvas inspector, which is what keeps
# fyne.io out of the backend's dependency graph. CI asserts the result is
# clean; use this file so local builds match.
set -euo pipefail

# shellcheck disable=SC2034
BACKEND_TAGS="headless"

# The backend needs cgo for the linker flags the platform layer uses.
export CGO_ENABLED="${CGO_ENABLED:-1}"
