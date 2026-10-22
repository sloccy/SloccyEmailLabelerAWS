#!/usr/bin/env bash
# Extra fixes run by the nightly security workflow (shared-ci security.yml extra_fix_script),
# after its own Go and npm fixes and before the tests.
set -euo pipefail
cd "$(dirname "$0")/.."

# Lambda Web Adapter layer (govulncheck cannot see it).
./scripts/lwa-autofix.sh

# If the npm step bumped Bootstrap/htmx in package.json, the pinned checksums must follow.
if ! git diff --quiet -- package.json; then
  echo "package.json changed: regenerating scripts/vendor.sha256"
  VENDOR_UPDATE_CHECKSUMS=1 ./scripts/vendor.sh
  {
    echo "Front-end vendor pins bumped; scripts/vendor.sha256 regenerated:"
    git diff -U0 -- package.json | grep -E '^[+-] +"(bootstrap|htmx\.org)"' | sed 's/^/  /'
  } >> "${SUMMARY_FILE:-/dev/stdout}"
fi
