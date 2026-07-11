#!/usr/bin/env bash
# Materializes pinned front-end deps into static/vendor/ (gitignored), which the Go
# binary embeds and serves at /static/vendor/ — the UI has no runtime CDN dependency.
#
# Versions live in package.json so Dependabot's npm ecosystem can bump them; this
# script fetches those exact versions from jsDelivr (same artifacts as the npm
# registry) so neither node nor npm is ever needed. Pre-gzipped copies are written
# alongside because server.go serves <asset>.gz when the client accepts gzip.
set -euo pipefail
cd "$(dirname "$0")/.."

ver() { sed -n "s/^ *\"$1\": *\"\([^\"]*\)\".*/\1/p" package.json; }
BOOTSTRAP="$(ver bootstrap)"
HTMX="$(ver 'htmx\.org')"
[ -n "$BOOTSTRAP" ] && [ -n "$HTMX" ] || { echo "vendor.sh: failed to parse versions from package.json" >&2; exit 1; }

mkdir -p static/vendor

fetch() { # $1=url $2=dest-name
  curl -fsSL "$1" -o "static/vendor/$2"
  gzip -kf9 "static/vendor/$2"
  echo "  vendor/$2 ($(wc -c <"static/vendor/$2") bytes)"
}

fetch "https://cdn.jsdelivr.net/npm/bootstrap@${BOOTSTRAP}/dist/css/bootstrap.min.css" bootstrap.min.css
fetch "https://cdn.jsdelivr.net/npm/bootstrap@${BOOTSTRAP}/dist/js/bootstrap.bundle.min.js" bootstrap.bundle.min.js
fetch "https://cdn.jsdelivr.net/npm/htmx.org@${HTMX}/dist/htmx.min.js" htmx.min.js
