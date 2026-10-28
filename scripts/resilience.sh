#!/usr/bin/env bash
# What Kubernetes buys: kill an API replica and the RabbitMQ pod in the middle of a sale and check that nothing is lost or done
# twice. Four fans each order two seats of a gig of eight; the prices paid must be exactly the ladder, whichever order the
# orders land in. Needs the cluster from `make cluster-up`.
set -euo pipefail

API="${API:-http://127.0.0.1:5233}"
CTX="${CTX:-kind-golang-encore}"
K="kubectl --context $CTX -n encore"
RUN="$(date +%s)$((RANDOM % 900 + 100))"
JSON='content-type: application/json'
say() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }
token_of() { python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"; }
# every call retries a few times: for a moment after a pod dies the Service may still send a request to it
retry() { for _ in $(seq 1 20); do if out="$("$@" 2>/dev/null)"; then printf '%s' "$out"; return 0; fi; sleep 1; done; return 1; }

login() { retry curl -sf -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"admin@encore.local\",\"password\":\"${ADMIN_PASSWORD:-EncoreAdmin2026}\"}" | token_of; }
register() { retry curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"$1-$RUN@example.com\",\"password\":\"Resil-Passw0rd1\"}" | token_of; }
ADM="Authorization: Bearer $(login)"
TOKENS=()
for n in a b c d; do TOKENS+=("Authorization: Bearer $(register $n)"); done

echo "1. a gig of eight seats (early bird 2, standard 4, last seats 2), and two orders of two seats in flight"
retry curl -sf -X POST "$API/api/gigs" -H "$JSON" -H "$ADM" -d "{\"code\":\"RES-$RUN\",\"name\":\"Resilience\",\"stage\":\"Main Stage\",\"band\":\"The Nimbus\",\"seatsTotal\":8,\"priceCents\":5000,\"maxPerFan\":4}" >/dev/null || fail "could not open the gig"
order() { retry curl -sf -X POST "$API/api/orders" -H "$JSON" -H "$1" -d "{\"gigCode\":\"RES-$RUN\",\"reference\":\"FAN-$RUN-$2\",\"seats\":2}" | jget "['id']"; }
IDS=("$(order "${TOKENS[0]}" a)" "$(order "${TOKENS[1]}" b)")

echo "2. kill an API replica and the RabbitMQ pod while the orders are still in flight"
$K delete "$($K get pods -l app.kubernetes.io/name=api -o name | head -1)" --wait=false | sed 's/^/  /'
$K delete pod rabbitmq-0 --wait=false | sed 's/^/  /'
IDS+=("$(order "${TOKENS[2]}" c)" "$(order "${TOKENS[3]}" d)") # accepted even now: the order and its outbox row are one local transaction
say "two more orders were accepted while the broker was down"

echo "3. everything comes back: every seat is sold exactly once and the prices are exactly the ladder"
$K rollout status statefulset/rabbitmq --timeout=180s >/dev/null
$K rollout status deployment/api --timeout=180s >/dev/null
field() { body="$(retry curl -sf -H "$1" "$API/api/orders/$2")" && printf '%s' "$body" | jget "['$3']" || echo unknown; }
for _ in $(seq 1 240); do
  done=0
  for i in 0 1 2 3; do [ "$(field "${TOKENS[$i]}" "${IDS[$i]}" status)" = Sold ] && done=$((done + 1)); done
  [ "$done" = 4 ] && break
  sleep 1
done
TOTALS=""
for i in 0 1 2 3; do
  [ "$(field "${TOKENS[$i]}" "${IDS[$i]}" status)" = Sold ] || fail "order $i is not sold: $(field "${TOKENS[$i]}" "${IDS[$i]}" status)"
  TOTALS="$TOTALS $(field "${TOKENS[$i]}" "${IDS[$i]}" totalCents)"
done
SORTED="$(printf '%s\n' $TOTALS | sort -n | tr '\n' ' ')"
[ "$SORTED" = "8960 11200 11200 14000 " ] || fail "the totals must be the ladder (8960 11200 11200 14000), got $SORTED"
GIG="$(retry curl -sf -H "$ADM" "$API/api/gigs/RES-$RUN")"
[ "$(printf '%s' "$GIG" | jget "['seatsLeft']")" = 0 ] || fail "the gig must be sold out"
[ "$(printf '%s' "$GIG" | jget "['tier']")" = None ] || fail "a sold-out gig has no next seat"
PASSES=0
for i in 0 1 2 3; do
  if curl -sf -H "$ADM" "$API/api/passes/${IDS[$i]}" >/dev/null 2>&1; then PASSES=$((PASSES + 1)); fi
done
[ "$PASSES" = 4 ] || fail "four passes must exist, found $PASSES"
say "totals $SORTED(the ladder), 0 seats left, 4 passes"
echo "resilience passed"
