#!/usr/bin/env bash
# Post-deploy smoke test: GET / on PushFunction's Function URL must return 200.
#
# push.go serves GET / as a health check (AuthType NONE, no auth), so a 200 proves the
# deployed Lambda Web Adapter layer starts, the binary boots under it, and the adapter
# proxies a request. PushFunction and WebFunction share the layer and the same binary.
# Retries absorb cold start / URL propagation right after a deploy.
#
# SMOKE_FORCE_FAIL=true makes this fail without calling anything — a drill switch for
# exercising the rollback path (deploy.yml's force_smoke_fail input).
set -euo pipefail

STACK=${STACK:-ollamail}

if [ "${SMOKE_FORCE_FAIL:-false}" = "true" ]; then
  echo "SMOKE_FORCE_FAIL set — failing on purpose" >&2
  exit 1
fi

url=$(aws cloudformation describe-stacks --stack-name "$STACK" \
  --query "Stacks[0].Outputs[?OutputKey=='PushFunctionUrl'].OutputValue" --output text)
[ -n "$url" ] && [ "$url" != "None" ] || { echo "no PushFunctionUrl output on stack $STACK" >&2; exit 1; }

for i in $(seq 1 6); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 "$url" || true)
  echo "attempt $i: GET $url -> $code"
  [ "$code" = "200" ] && exit 0
  sleep 10
done
echo "smoke test failed: no 200 from $url" >&2
exit 1
