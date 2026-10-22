#!/usr/bin/env bash
# Materializes pinned front-end deps into static/vendor/ (gitignored), which the Go
# binary embeds and serves at /static/vendor/ — the UI has no runtime CDN dependency.
#
# Versions live in package.json (the nightly security job bumps them to the minimum fixed
# version and regenerates vendor.sha256); this
# script fetches those exact versions from jsDelivr (same artifacts as the npm
# registry) so neither node nor npm is ever needed. Pre-gzipped copies are written
# alongside because server.go serves <asset>.gz when the client accepts gzip.
#
# Each fetch is checked against vendor.sha256 before being embedded — a compromised
# CDN or MITM'd fetch would otherwise ship attacker JS straight into the deployed
# binary with nothing else in the pipeline to catch it (there's no npm lockfile,
# by design). When you bump BOOTSTRAP/HTMX in package.json, regenerate
# vendor.sha256 in the same commit (this script's error message says how) — the two
# are meant to move together, not drift.
set -euo pipefail
cd "$(dirname "$0")/.."

ver() { sed -n "s/^ *\"$1\": *\"\([^\"]*\)\".*/\1/p" package.json; }
BOOTSTRAP="$(ver bootstrap)"
HTMX="$(ver 'htmx\.org')"
[ -n "$BOOTSTRAP" ] && [ -n "$HTMX" ] || { echo "vendor.sh: failed to parse versions from package.json" >&2; exit 1; }

mkdir -p static/vendor

# VENDOR_UPDATE_CHECKSUMS=1 rewrites scripts/vendor.sha256 from what was just fetched instead
# of verifying against it. The nightly security job uses this after it bumps a version in
# package.json; do not set it for a normal build.
NEWSUMS="$(mktemp)"
trap 'rm -f "$NEWSUMS"' EXIT

fetch() { # $1=url $2=dest-name
  curl -fsSL "$1" -o "static/vendor/$2"
  if [ "${VENDOR_UPDATE_CHECKSUMS:-}" = "1" ]; then
    (cd static/vendor && sha256sum "$2") >> "$NEWSUMS"
  elif ! (cd static/vendor && grep " $2\$" ../../scripts/vendor.sha256 | sha256sum -c - >/dev/null); then
    echo "vendor.sh: checksum mismatch for $2 — refusing to embed it in the build." >&2
    echo "  If this is an intentional version bump, regenerate scripts/vendor.sha256:" >&2
    echo "    sha256sum -b static/vendor/$2 | sed 's/\*//' " >&2
    exit 1
  fi
  # -n omits the mtime and original filename from the gzip header. Without it every
  # run produces different bytes for identical input, and since go:embed bakes these
  # into the binary, SAM's four per-function builds each yield a distinct artifact
  # and upload their own ~9 MiB copy of the same program.
  gzip -nkf9 "static/vendor/$2"
  echo "  vendor/$2 ($(wc -c <"static/vendor/$2") bytes, checksum OK)"
}

fetch "https://cdn.jsdelivr.net/npm/bootstrap@${BOOTSTRAP}/dist/css/bootstrap.min.css" bootstrap.min.css
fetch "https://cdn.jsdelivr.net/npm/bootstrap@${BOOTSTRAP}/dist/js/bootstrap.bundle.min.js" bootstrap.bundle.min.js
fetch "https://cdn.jsdelivr.net/npm/htmx.org@${HTMX}/dist/htmx.min.js" htmx.min.js

if [ "${VENDOR_UPDATE_CHECKSUMS:-}" = "1" ]; then
  sort -k2 "$NEWSUMS" > scripts/vendor.sha256
  echo "  scripts/vendor.sha256 rewritten"
fi
