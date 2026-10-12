#!/usr/bin/env bash
# Conservative auto-bump for the Lambda Web Adapter layer (template.yaml).
#
# govulncheck and Dependabot can't see this layer, so this is its equivalent: act only when
# upstream has PUBLISHED a security advisory that the pinned release is still vulnerable to,
# and then bump to the lowest layer version that carries the fix — never to "latest".
#
# The layer version <-> release mapping comes from upstream's README at each release tag,
# which names that release's arm64 layer ARN. Anything we can't map or parse exits non-zero
# (the workflow turns that into a failed run + email) rather than guessing.
#
# Prints "bumped=<old>-><new> (<release>, <GHSA ids>)" to $GITHUB_OUTPUT when it edits
# template.yaml; prints nothing and leaves the file alone when no bump is needed.
set -euo pipefail

REPO=awslabs/aws-lambda-web-adapter
TEMPLATE=${TEMPLATE:-template.yaml}
OUT=${GITHUB_OUTPUT:-/dev/stdout}

current=$(awk '/^  LambdaWebAdapterLayerVersion:/{f=1} f&&/Default:/{print $2; exit}' "$TEMPLATE")
[[ "$current" =~ ^[0-9]+$ ]] || { echo "could not read pinned layer version from $TEMPLATE" >&2; exit 1; }

layer_at_tag() {
  gh api "repos/$REPO/contents/README.md?ref=$1" --jq .content | base64 -d \
    | grep -o 'LambdaAdapterLayerArm64:[0-9]*' | head -1 | cut -d: -f2
}

# release tag -> layer version, stable releases only (no -rc), semver-sorted ascending
mapfile -t tags < <(gh release list -R "$REPO" -L 100 --exclude-pre-releases --exclude-drafts \
  --json tagName --jq '.[].tagName' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V)
declare -A layer_of
for t in "${tags[@]}"; do
  l=$(layer_at_tag "$t" || true)
  [[ "$l" =~ ^[0-9]+$ ]] && layer_of[$t]=$l
done

# Current release = newest release whose layer version is <= ours (exact match normally).
cur_tag=""
for t in "${tags[@]}"; do
  [ -n "${layer_of[$t]:-}" ] && [ "${layer_of[$t]}" -le "$current" ] && cur_tag=$t
done
[ -n "$cur_tag" ] || { echo "layer $current maps to no known release" >&2; exit 1; }
echo "pinned layer $current ~ $cur_tag"

# Highest patched version across published, non-withdrawn advisories newer than our release.
need=""; ids=""
while IFS=$'\t' read -r ghsa patched; do
  [ -n "$patched" ] || continue
  p="v${patched#v}"
  if [ "$(printf '%s\n%s\n' "$cur_tag" "$p" | sort -V | tail -1)" = "$p" ] && [ "$p" != "$cur_tag" ]; then
    ids="$ids $ghsa"
    need=$(printf '%s\n%s\n' "${need:-$p}" "$p" | sort -V | tail -1)
  fi
done < <(gh api "repos/$REPO/security-advisories" --paginate \
  --jq '.[] | select(.state=="published" and .withdrawn_at==null) | [.ghsa_id, (.vulnerabilities[0].patched_versions // "")] | @tsv')

if [ -z "$need" ]; then
  echo "no unfixed published advisories for $cur_tag — nothing to do"
  exit 0
fi

new=${layer_of[$need]:-}
[ -n "$new" ] || { echo "advisory fixed in $need but no layer version maps to it" >&2; exit 1; }
[ "$new" -gt "$current" ] || { echo "mapping for $need ($new) is not newer than pinned $current" >&2; exit 1; }

sed -i "/^  LambdaWebAdapterLayerVersion:/,/Default:/ s/Default: $current/Default: $new/" "$TEMPLATE"
echo "bumped=layer $current -> $new ($need; fixes${ids})" >> "$OUT"
echo "bumped layer $current -> $new for $need"
