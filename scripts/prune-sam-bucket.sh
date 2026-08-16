#!/usr/bin/env bash
# Deletes SAM deployment artifacts that no live CloudFormation stack references.
#
# `sam deploy` uploads a content-hashed copy of each function's zip (~8 MiB, deduped to
# one object by the Makefile's -trimpath) plus the packaged template on every deploy, and
# never removes the previous set — the bucket grows without bound. Everything here is
# reproducible from git, so the only copies worth keeping are the ones the deployed stacks
# still point at: CloudFormation needs them to update or roll back a function, and nothing
# needs them after that.
#
# The SAM managed bucket has versioning ENABLED (SAM turns it on when it creates the
# bucket). That makes the obvious implementation silently useless: list-objects-v2 shows
# only current versions, and delete-objects without a VersionId writes a delete marker
# instead of freeing anything. An earlier version of this script did exactly that — it
# reported "nothing to prune" against a bucket holding 2x its live bytes in noncurrent
# versions. So everything below works in (Key, VersionId) space, not key space.
#
# Content-hashed keys mean a key's contents never change, so any noncurrent version of a
# referenced key is a byte-identical re-upload from an earlier deploy. Keeping just the
# current version of each referenced key loses nothing.
#
# Deliberately not an age-based lifecycle rule: an Expiration can't tell a superseded
# artifact from the one that's currently deployed, so it happily deletes a live zip during
# any quiet stretch and strands the next failed deploy in UPDATE_ROLLBACK_FAILED.
# Reachability is the correct criterion, so we compute it. (There *is* a
# NoncurrentVersionExpiration rule on the bucket as a backstop for deploys where this
# script doesn't run — that one is safe precisely because a version only becomes
# noncurrent once something newer has replaced it.)
#
# Scans every stack in the region, not just this one, so a second SAM app sharing the
# managed bucket doesn't get its artifacts pruned out from under it.
set -euo pipefail
cd "$(dirname "$0")/.."

REGION="${AWS_REGION:-us-east-1}"
DRY_RUN="${DRY_RUN:-0}"

BUCKET=$(aws cloudformation describe-stacks \
  --stack-name aws-sam-cli-managed-default --region "$REGION" \
  --query 'Stacks[0].Outputs[?OutputKey==`SourceBucket`].OutputValue' --output text)
[ -n "$BUCKET" ] && [ "$BUCKET" != "None" ] || { echo "could not resolve SAM bucket" >&2; exit 1; }

# Every stack that could hold a reference. A stack mid-rollback still points at the
# artifacts it may need to restore, so include the in-progress and failed states too.
stacks=$(aws cloudformation list-stacks --region "$REGION" \
  --stack-status-filter CREATE_COMPLETE CREATE_IN_PROGRESS CREATE_FAILED \
    UPDATE_COMPLETE UPDATE_IN_PROGRESS UPDATE_FAILED \
    UPDATE_ROLLBACK_COMPLETE UPDATE_ROLLBACK_IN_PROGRESS UPDATE_ROLLBACK_FAILED \
    ROLLBACK_COMPLETE ROLLBACK_IN_PROGRESS ROLLBACK_FAILED REVIEW_IN_PROGRESS \
    IMPORT_COMPLETE IMPORT_ROLLBACK_COMPLETE \
  --query 'StackSummaries[].StackName' --output text)

# S3Key covers function code; the bare-hash and TemplateURL forms cover nested-stack
# templates. Matching hex basenames off the raw template text catches all of them
# whether CloudFormation hands the body back as JSON or YAML.
referenced=$(
  for s in $stacks; do
    aws cloudformation get-template --stack-name "$s" --region "$REGION" \
      --template-stage Original --output text 2>/dev/null || true
    aws cloudformation get-template --stack-name "$s" --region "$REGION" \
      --template-stage Processed --output text 2>/dev/null || true
  done | grep -oE '[0-9a-f]{32}(\.template)?' | sort -u
)

# A failed scan must never look like "nothing is referenced" — that would delete the
# live artifacts. If any stack exists, it has to have yielded at least one key.
if [ -n "$stacks" ] && [ -z "$referenced" ]; then
  echo "refusing to prune: found stacks but resolved no artifact references" >&2
  exit 1
fi

ref_json=$(printf '%s\n' "$referenced" | jq -R -s -c 'split("\n") | map(select(length>0))')
versions=$(aws s3api list-object-versions --bucket "$BUCKET" --region "$REGION" --output json)

# Keep exactly one thing per referenced key: its current version. Everything else goes —
# noncurrent versions of referenced keys (byte-identical re-uploads), every version of an
# unreferenced key, and every delete marker (including the ones the old key-space
# implementation left behind).
keep=$(echo "$versions" | jq -c --argjson ref "$ref_json" \
  '[ .Versions[]? | . as $v | select($v.IsLatest and (($ref | index($v.Key)) != null)) ]')
doomed=$(echo "$versions" | jq -c --argjson ref "$ref_json" \
  '[ ( .Versions[]? | . as $v
       | select( ($v.IsLatest and (($ref | index($v.Key)) != null)) | not ) ),
     ( .DeleteMarkers[]? ) ] | map({Key, VersionId})')

# Every referenced key must survive with a live version. If one doesn't, the bucket and
# the deployed stacks disagree and deleting anything now would make it worse.
missing=$(jq -rn --argjson ref "$ref_json" --argjson keep "$keep" \
  '$ref - ($keep | map(.Key)) | .[]')
if [ -n "$missing" ]; then
  echo "refusing to prune: referenced artifact(s) have no current version in $BUCKET:" >&2
  echo "$missing" | sed 's/^/  /' >&2
  exit 1
fi

count=$(echo "$doomed" | jq 'length')
freed=$(echo "$versions" | jq -r --argjson ref "$ref_json" \
  '[ .Versions[]? | . as $v
     | select( ($v.IsLatest and (($ref | index($v.Key)) != null)) | not ) | $v.Size ] | add // 0')

if [ "$count" -eq 0 ]; then
  echo "nothing to prune ($(echo "$keep" | jq 'length') live object(s), no stale versions)"
  exit 0
fi

if [ "$DRY_RUN" = "1" ]; then
  echo "would delete $count stale version(s)/marker(s) from $BUCKET, freeing $freed bytes:"
  echo "$doomed" | jq -r '.[] | "  \(.Key) \(.VersionId)"'
  exit 0
fi

# delete-objects caps at 1000 keys per call.
echo "$doomed" | jq -c '[range(0; length; 1000) as $i | .[$i:$i+1000]] | .[]' | while read -r batch; do
  echo "$batch" | jq -c '{Objects: ., Quiet: true}' > "/tmp/sam-prune-$$.json"
  aws s3api delete-objects --bucket "$BUCKET" --region "$REGION" \
    --delete "file:///tmp/sam-prune-$$.json" --output text >/dev/null
  rm -f "/tmp/sam-prune-$$.json"
done

echo "pruned $count stale version(s)/marker(s) from $BUCKET, freed $freed bytes"
