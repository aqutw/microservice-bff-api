#!/usr/bin/env bash
# Run against a dedicated Compose project, with freshly created volumes.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
for executable in curl jq docker; do
  command -v "$executable" >/dev/null || { echo "Missing prerequisite: $executable" >&2; exit 1; }
done

artifacts_dir=artifacts/sit
mkdir -p "$artifacts_dir"
run_id="sit-$(date +%s)-$$"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

wait_http() {
  local url=$1
  for ((attempt=0; attempt<60; attempt++)); do
    if curl --fail --silent --max-time 3 "$url" >/dev/null; then return; fi
    sleep 1
  done
  fail "Service did not become ready: $url"
}

# Capture both status and response body; expected business errors are assertions too.
call() {
  local expected=$1 label=$2 method=$3 url=$4 correlation=$5 payload=${6:-}
  local status
  local args=(--silent --show-error --max-time 10 --request "$method"
    --header 'content-type: application/json'
    --header 'x-actor-id: sit-runner' --header "x-correlation-id: $correlation"
    --output "$artifacts_dir/$label.json" --write-out '%{http_code}')
  if [[ -n "$payload" ]]; then args+=(--data "$payload"); fi
  status=$(curl "${args[@]}" "$url")
  [[ "$status" == "$expected" ]] || fail "$label returned $status, expected $expected (see $label.json)"
}

assert_json() { jq -e "$2" "$artifacts_dir/$1.json" >/dev/null || fail "$1: $2"; }
stock() { curl --fail --silent --show-error --max-time 5 "http://localhost:8082/v1/stock/$1" | jq -er .available; }

wait_audit() {
  local correlation=$1 expected=$2 label=$3
  for ((attempt=0; attempt<60; attempt++)); do
    if curl --fail --silent --max-time 3 \
      "http://localhost:3001/api/audit/events?correlation_id=$correlation&limit=100" \
      > "$artifacts_dir/$label.json" && \
      jq -e --argjson expected "$expected" '.count == $expected and (.events | length) == $expected' \
      "$artifacts_dir/$label.json" >/dev/null; then return; fi
    sleep 1
  done
  fail "Audit delivery timed out for $correlation; expected $expected events"
}

for port in 8081 8082 8083 8084 3001 3002; do
  wait_http "http://localhost:$port/healthz"
  call 200 "health-$port" GET "http://localhost:$port/healthz" "$run_id"
  assert_json "health-$port" '.status == "ok"'
done
wait_http http://localhost:3000
curl --fail --silent --show-error --max-time 10 http://localhost:3000/audit > "$artifacts_dir/dashboard.html"
pass 'All APIs, storefront and audit dashboard respond'

before=$(stock mug)
correlation="$run_id-desktop"
call 201 desktop POST http://localhost:3001/api/checkout "$correlation" \
  '{"user_id":"sit-desktop","items":[{"sku":"mug","quantity":1,"unit_price_cents":650}]}'
assert_json desktop '.order.status == "pending_payment" and .order.total.cents == 650 and (.payment.redirectUrl | type) == "string"'
order_id=$(jq -er .order.id "$artifacts_dir/desktop.json")
payment_id=$(jq -er .payment.id "$artifacts_dir/desktop.json")
reservation_id=$(jq -er .inventory.reservationId "$artifacts_dir/desktop.json")
[[ $(stock mug) -eq $((before - 1)) ]] || fail 'Desktop checkout did not reserve stock'
call 200 desktop-confirm POST "http://localhost:3001/api/payments/$payment_id/confirm" "$correlation"
assert_json desktop-confirm '.payment.status == "paid" and .orderStatus == "paid"'
call 200 desktop-order GET "http://localhost:3001/api/orders/$order_id" "$correlation"
assert_json desktop-order '.status == "paid" and .total_cents == 650'
call 409 duplicate-confirm POST "http://localhost:3001/api/payments/$payment_id/confirm" "$correlation"
assert_json duplicate-confirm '.error == "payment_not_pending"'
call 200 desktop-commit POST "http://localhost:8082/v1/reservations/$reservation_id/commit" "$correlation"
assert_json desktop-commit '.status == "committed"'
wait_audit "$correlation" 6 desktop-audit
assert_json desktop-audit '([.events[].service] | unique | sort) == ["inventory","order","payment"] and all(.events[]; .actor_id == "sit-runner")'
pass 'Desktop checkout, payment, inventory commit and correlated audit delivery'

correlation="$run_id-app"
call 201 app POST http://localhost:3002/api/checkout "$correlation" \
  '{"user_id":"sit-app","items":[{"sku":"tea-leaves","quantity":1,"unit_price_cents":280}]}'
assert_json app '.state == "awaiting_payment" and .amount.cents == 280 and (.payment.clientToken | type) == "string" and (.payment | has("redirectUrl") | not)'
payment_id=$(jq -er .payment.id "$artifacts_dir/app.json")
call 200 app-confirm POST "http://localhost:3002/api/payments/$payment_id/confirm" "$correlation"
assert_json app-confirm '.state == "paid"'
wait_audit "$correlation" 5 app-audit
pass 'App checkout and native payment contract'

before=$(stock mug)
correlation="$run_id-stock-failure"
call 409 insufficient-stock POST http://localhost:3001/api/checkout "$correlation" \
  '{"user_id":"sit-failure","items":[{"sku":"mug","quantity":999,"unit_price_cents":650}]}'
assert_json insufficient-stock '.error == "insufficient_stock"'
[[ $(stock mug) -eq $before ]] || fail 'Insufficient stock changed available quantity'
wait_audit "$correlation" 2 stock-failure-audit
assert_json stock-failure-audit 'any(.events[]; .service == "order" and .after.status == "cancelled")'
pass 'Stock rejection compensates the order without changing inventory'

before=$(stock coffee-beans)
docker compose stop payment
correlation="$run_id-payment-failure"
call 502 payment-failure POST http://localhost:3001/api/checkout "$correlation" \
  '{"user_id":"sit-failure","items":[{"sku":"coffee-beans","quantity":1,"unit_price_cents":420}]}'
[[ $(stock coffee-beans) -eq $before ]] || fail 'Payment failure did not release inventory'
wait_audit "$correlation" 4 payment-failure-audit
assert_json payment-failure-audit 'any(.events[]; .event_type == "inventory.released") and any(.events[]; .service == "order" and .after.status == "cancelled")'
docker compose start payment
wait_http http://localhost:8083/healthz
pass 'Payment service outage releases inventory and cancels the order'

docker compose stop audit-worker
correlation="$run_id-audit-outage"
call 201 outage-checkout POST http://localhost:3001/api/checkout "$correlation" \
  '{"user_id":"sit-audit-outage","items":[{"sku":"coffee-beans","quantity":1,"unit_price_cents":420}]}'
assert_json outage-checkout '.order.status == "pending_payment"'
# Restart producers before recovery: queued events must survive in SQLite.
docker compose restart order inventory payment
for port in 8081 8082 8083; do wait_http "http://localhost:$port/healthz"; done
docker compose start audit-worker
wait_http http://localhost:8084/healthz
wait_audit "$correlation" 3 outage-audit
assert_json outage-audit '([.events[].service] | unique | sort) == ["inventory","order","payment"]'
pass 'Checkout succeeds during audit outage; persisted outbox delivers after restart'

event_payload=$(jq -c '.events[0]' "$artifacts_dir/outage-audit.json")
call 202 audit-replay POST http://localhost:8084/v1/audit/events "$correlation" "$event_payload"
call 200 audit-after-replay GET "http://localhost:3001/api/audit/events?correlation_id=$correlation" "$correlation"
assert_json audit-after-replay '.count == 3'
call 200 audit-filter GET "http://localhost:3001/api/audit/events?service=payment&correlation_id=$correlation&limit=1" "$correlation"
assert_json audit-filter '.count == 1 and .events[0].service == "payment"'
pass 'Audit Store deduplicates repeated delivery and query filters work'

echo 'SIT passed'
