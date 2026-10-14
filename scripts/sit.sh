#!/usr/bin/env bash
# Run against a dedicated Compose project, with freshly created volumes.
set -euo pipefail

DESKTOP_BFF_URL=${DESKTOP_BFF_URL:-https://localhost:3001}
APP_BFF_URL=${APP_BFF_URL:-https://localhost:3002}
ORDER_API_URL=${ORDER_API_URL:-https://localhost:8081}
INVENTORY_API_URL=${INVENTORY_API_URL:-https://localhost:8082}
PAYMENT_API_URL=${PAYMENT_API_URL:-https://localhost:8083}
AUDIT_API_URL=${AUDIT_API_URL:-https://localhost:8084}
WEBSITE_URL=${WEBSITE_URL:-http://localhost:3000}

cd "$(dirname "${BASH_SOURCE[0]}")/.."
export CURL_CA_BUNDLE=${CURL_CA_BUNDLE:-"$PWD/.security/pki/ca.pem"}
for executable in curl jq docker node; do
  command -v "$executable" >/dev/null || { echo "Missing prerequisite: $executable" >&2; exit 1; }
done

SECURITY_ENV_FILE=${SECURITY_ENV_FILE:-.env.security}
export SECURITY_ENV_FILE
node scripts/test-tokens.mjs "$SECURITY_ENV_FILE"
set -a
source .security/tokens.env
set +a
internal_headers() { node scripts/request-headers.mjs "$@"; }

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
    --header "authorization: Bearer $SIT_TOKEN" --header "x-correlation-id: $correlation"
    --header "idempotency-key: $correlation-$label"
    --output "$artifacts_dir/$label.json" --write-out '%{http_code}')
  local target="" caller=order
  if [[ "$url" == "$INVENTORY_API_URL"/* ]]; then target=inventory; fi
  if [[ "$url" == "$AUDIT_API_URL"/* && "$method" == POST ]]; then target=audit-worker; caller=$(jq -r .service <<< "$payload"); fi
  if [[ -n "$target" ]]; then
    while IFS= read -r header; do args+=(--header "$header"); done < <(internal_headers "$target" "$caller" "$method" "$url" "$payload" "$correlation" "$correlation-$label")
  fi
  if [[ -n "$payload" ]]; then args+=(--data "$payload"); fi
  status=$(curl "${args[@]}" "$url")
  # Checkout may be claimed by recovery before the initiating HTTP request.
  # Wait for its business outcome while retaining the acceptance response.
  if [[ "$status" == 202 && "$expected" != 202 && "$url" == */api/checkout ]]; then
    local checkout_id
    checkout_id=$(jq -er .checkoutId "$artifacts_dir/$label.json")
    cp "$artifacts_dir/$label.json" "$artifacts_dir/$label-accepted.json"
    for attempt in {1..90}; do
      status=$(curl --silent --show-error --header "authorization: Bearer $SIT_TOKEN" --max-time 5 --output "$artifacts_dir/$label.json" --write-out '%{http_code}' "${url%/api/checkout}/api/checkouts/$checkout_id")
      [[ "$status" != 202 ]] && break
      sleep 1
    done
    if [[ "$expected" == 201 && "$status" == 200 ]]; then status=201; fi
  fi
  if [[ "$status" == 202 && "$expected" == 200 && "$url" == */confirm ]]; then
    local checkout_id poll_base
    checkout_id=$(jq -er .checkoutId "$artifacts_dir/$label.json")
    poll_base=${url%%/api/payments/*}
    cp "$artifacts_dir/$label.json" "$artifacts_dir/$label-accepted.json"
    for attempt in {1..90}; do
      status=$(curl --silent --show-error --header "authorization: Bearer $SIT_TOKEN" --max-time 5 --output "$artifacts_dir/$label.json" --write-out '%{http_code}' "$poll_base/api/checkouts/$checkout_id")
      [[ "$status" != 202 ]] && break
      sleep 1
    done
    if [[ "$status" == 200 ]]; then
      jq '. + {orderStatus: (.order.status // .state)}' "$artifacts_dir/$label.json" > "$artifacts_dir/$label-settled.json"
      mv "$artifacts_dir/$label-settled.json" "$artifacts_dir/$label.json"
    fi
  fi
  [[ "$status" == "$expected" ]] || fail "$label returned $status, expected $expected (see $label.json)"
}

assert_json() { jq -e "$2" "$artifacts_dir/$1.json" >/dev/null || fail "$1: $2"; }
stock() {
 local url="${INVENTORY_API_URL}/v1/stock/$1" args=()
 while IFS= read -r header; do args+=(--header "$header"); done < <(internal_headers inventory order GET "$url")
 curl --fail --silent --show-error --max-time 5 "${args[@]}" "$url" | jq -er .available
}

wait_audit() {
  local correlation=$1 expected=$2 label=$3
  for ((attempt=0; attempt<60; attempt++)); do
    if curl --fail --silent --header "authorization: Bearer $SIT_TOKEN" --max-time 3 \
      "${DESKTOP_BFF_URL}/api/audit/events?correlation_id=$correlation&limit=100" \
      > "$artifacts_dir/$label.json" && \
      jq -e --argjson expected "$expected" '.count == $expected and (.events | length) == $expected' \
      "$artifacts_dir/$label.json" >/dev/null; then return; fi
    sleep 1
  done
  fail "Audit delivery timed out for $correlation; expected $expected events"
}

for entry in "order:$ORDER_API_URL" "inventory:$INVENTORY_API_URL" "payment:$PAYMENT_API_URL" "audit:$AUDIT_API_URL" "desktop:$DESKTOP_BFF_URL" "app:$APP_BFF_URL"; do
  name=${entry%%:*}
  url=${entry#*:}
  wait_http "$url/healthz"
  call 200 "health-$name" GET "$url/healthz" "$run_id"
  assert_json "health-$name" '.status == "ok"'
done
wait_http ${WEBSITE_URL}
curl --fail --silent --show-error --max-time 10 ${WEBSITE_URL}/audit > "$artifacts_dir/dashboard.html"
pass 'All APIs, storefront and audit dashboard respond'

before=$(stock mug)
correlation="$run_id-desktop"
call 201 desktop POST ${DESKTOP_BFF_URL}/api/checkout "$correlation" \
  '{"user_id":"sit-desktop","items":[{"sku":"mug","quantity":1,"unit_price_cents":650}]}'
assert_json desktop '.order.status == "pending_payment" and .order.total.cents == 650 and (.payment.redirectUrl | type) == "string"'
order_id=$(jq -er .order.id "$artifacts_dir/desktop.json")
payment_id=$(jq -er .payment.id "$artifacts_dir/desktop.json")
reservation_id=$(jq -er .inventory.reservationId "$artifacts_dir/desktop.json")
[[ $(stock mug) -eq $((before - 1)) ]] || fail 'Desktop checkout did not reserve stock'
call 200 desktop-confirm POST "${DESKTOP_BFF_URL}/api/payments/$payment_id/confirm" "$correlation"
assert_json desktop-confirm '.payment.status == "paid" and .orderStatus == "paid"'
call 200 desktop-order GET "${DESKTOP_BFF_URL}/api/orders/$order_id" "$correlation"
assert_json desktop-order '.status == "paid" and .total_cents == 650'
call 200 duplicate-confirm POST "${DESKTOP_BFF_URL}/api/payments/$payment_id/confirm" "$correlation"
assert_json duplicate-confirm '.payment.status == "paid" and .orderStatus == "paid"'
call 200 desktop-commit POST "${INVENTORY_API_URL}/v1/reservations/$reservation_id/commit" "$correlation" "{\"order_id\":\"$order_id\"}"
assert_json desktop-commit '.status == "committed"'
wait_audit "$correlation" 6 desktop-audit
assert_json desktop-audit '([.events[].service] | unique | sort) == ["inventory","order","payment"] and all(.events[]; (.actor_id | startswith("usr_")))'
pass 'Desktop checkout, payment, inventory commit and correlated audit delivery'

correlation="$run_id-app"
call 201 app POST ${APP_BFF_URL}/api/checkout "$correlation" \
  '{"user_id":"sit-app","items":[{"sku":"tea-leaves","quantity":1,"unit_price_cents":280}]}'
assert_json app '.state == "awaiting_payment" and .amount.cents == 280 and (.payment.clientToken | type) == "string" and (.payment | has("redirectUrl") | not)'
payment_id=$(jq -er .payment.id "$artifacts_dir/app.json")
call 200 app-confirm POST "${APP_BFF_URL}/api/payments/$payment_id/confirm" "$correlation"
assert_json app-confirm '.state == "paid"'
wait_audit "$correlation" 6 app-audit
pass 'App checkout and native payment contract'

before=$(stock mug)
correlation="$run_id-stock-failure"
call 409 insufficient-stock POST ${DESKTOP_BFF_URL}/api/checkout "$correlation" \
  '{"user_id":"sit-failure","items":[{"sku":"mug","quantity":999,"unit_price_cents":650}]}'
assert_json insufficient-stock '.error == "insufficient_stock"'
[[ $(stock mug) -eq $before ]] || fail 'Insufficient stock changed available quantity'
wait_audit "$correlation" 2 stock-failure-audit
assert_json stock-failure-audit 'any(.events[]; .service == "order" and .after.status == "cancelled")'
pass 'Stock rejection compensates the order without changing inventory'

before=$(stock coffee-beans)
docker compose stop payment
correlation="$run_id-payment-failure"
call 202 payment-failure POST ${DESKTOP_BFF_URL}/api/checkout "$correlation" \
  '{"user_id":"sit-failure","items":[{"sku":"coffee-beans","quantity":1,"unit_price_cents":420}]}'
assert_json payment-failure '.state == "processing" and (.checkoutId | type) == "string"'
checkout_id=$(jq -er .checkoutId "$artifacts_dir/payment-failure.json")
for attempt in {1..30}; do
  [[ $(stock coffee-beans) -eq $((before - 1)) ]] && break
  sleep 1
done
[[ $(stock coffee-beans) -eq $((before - 1)) ]] || fail 'Pending checkout did not keep its reservation'
docker compose restart order
wait_http ${ORDER_API_URL}/healthz
docker compose start payment
wait_http ${PAYMENT_API_URL}/healthz
for attempt in {1..90}; do
  status=$(curl --silent --show-error --header "authorization: Bearer $SIT_TOKEN" --max-time 5 --output "$artifacts_dir/payment-recovery.json" --write-out '%{http_code}' "${DESKTOP_BFF_URL}/api/checkouts/$checkout_id")
  if [[ "$status" == 200 ]] && jq -e '.state == "awaiting_payment"' "$artifacts_dir/payment-recovery.json" >/dev/null; then break; fi
  sleep 1
done
assert_json payment-recovery '.state == "awaiting_payment" and (.payment.id | type) == "string"'
[[ $(stock coffee-beans) -eq $((before - 1)) ]] || fail 'Recovery deducted stock more than once'
wait_audit "$correlation" 3 payment-failure-audit
pass 'Payment outage leaves a durable workflow that resumes after Order restart'

docker compose stop audit-worker
correlation="$run_id-audit-outage"
call 201 outage-checkout POST ${DESKTOP_BFF_URL}/api/checkout "$correlation" \
  '{"user_id":"sit-audit-outage","items":[{"sku":"coffee-beans","quantity":1,"unit_price_cents":420}]}'
assert_json outage-checkout '.order.status == "pending_payment"'
# Restart producers before recovery: queued events must survive in SQLite.
docker compose restart order inventory payment
for url in "$ORDER_API_URL" "$INVENTORY_API_URL" "$PAYMENT_API_URL"; do wait_http "$url/healthz"; done
docker compose start audit-worker
wait_http ${AUDIT_API_URL}/healthz
wait_audit "$correlation" 3 outage-audit
assert_json outage-audit '([.events[].service] | unique | sort) == ["inventory","order","payment"]'
pass 'Checkout succeeds during audit outage; persisted outbox delivers after restart'

event_payload=$(jq -c '.events[0]' "$artifacts_dir/outage-audit.json")
call 202 audit-replay POST ${AUDIT_API_URL}/v1/audit/events "$correlation" "$event_payload"
call 200 audit-after-replay GET "${DESKTOP_BFF_URL}/api/audit/events?correlation_id=$correlation" "$correlation"
assert_json audit-after-replay '.count == 3'
call 200 audit-filter GET "${DESKTOP_BFF_URL}/api/audit/events?service=payment&correlation_id=$correlation&limit=1" "$correlation"
assert_json audit-filter '.count == 1 and .events[0].service == "payment"'
pass 'Audit Store deduplicates repeated delivery and query filters work'

echo 'SIT passed'
