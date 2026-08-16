#!/usr/bin/env bash
# Deletes SAM deployment artifacts that no live CloudFormation stack references.
#
# `sam deploy` uploads a content-hashed copy of each function's zip (~9 MiB x 4) plus
# the packaged template on every deploy, and never removes the previous set — the
# bucket grows without bound. Everything here is reproducible from git, so the only
# copies worth keeping are the ones the deployed stacks still point at: CloudFormation
# needs them to update or roll back a function, and nothing needs them after that.
#
# Deliberately not a lifecycle rule: an age-based Expiration can't tell a superseded
# artifact from the one that's currently deployed, so it happily deletes a live zip
# during any quiet stretch and strands the next failed deploy in UPDATE_ROLLBACK_FAILED.
# Reachability is the correct criterion, so we compute it.
#
# Scans every stack in the region, not just this one, so a second SAM app sharing the
# managed bucket doesn't get its artifacts pruned out from under it.
set -euo pipefail
cd "$(dirname "$0")/.."

REGION="${AWS_REGION:-us-east-2}"
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

all=$(aws s3api list-objects-v2 --bucket "$BUCKET" --region "$REGION" \
  --query 'Contents[].Key' --output text | tr '\t' '\n' | sed '/^$/d' | sort -u)

orphans=$(comm -23 <(echo "$all") <(echo "$referenced") || true)

if [ -z "$orphans" ]; then
  echo "nothing to prune ($(echo "$all" | grep -c . ) objects, all referenced)"
  exit 0
fi

count=$(echo "$orphans" | grep -c .)
if [ "$DRY_RUN" = "1" ]; then
  echo "would delete $count orphaned artifact(s) from $BUCKET:"
  echo "$orphans" | sed 's/^/  /'
  exit 0
fi

echo "$orphans" | jq -R -s -c '{Objects: (split("\n") | map(select(length>0) | {Key: .}))}' \
  > /tmp/sam-prune-$$.json
aws s3api delete-objects --bucket "$BUCKET" --region "$REGION" \
  --delete "file:///tmp/sam-prune-$$.json" --output text >/dev/null
rm -f /tmp/sam-prune-$$.json

echo "pruned $count orphaned artifact(s) from $BUCKET"
