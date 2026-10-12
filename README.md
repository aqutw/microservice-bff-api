# Commerce microservices PoC

![Commerce microservices PoC](screenshot.jpg)

依照 `_/訂單服務 庫存 金流紀錄，用3個微服務實現嗎？.md` 與 `_/web與app; go與nodejs.md`，這個 PoC 將前端通道與核心領域拆開：

- `website/`：Next.js storefront，預設呼叫 desktop BFF。
- `bff-api/`：Node.js 20，`desktop.js` 在 3001、`app.js` 在 3002；兩者共用編排邏輯，但回傳給 Web／App 的資料形狀不同。
- `services/order/`：Go＋SQLite，建立訂單與狀態流轉。
- `services/inventory/`：Go＋SQLite，預扣、釋放、提交庫存。
- `services/payment/`：Go＋SQLite，建立待付款交易與 demo confirm。
- `services/audit-worker/`：Go＋SQLite，接收各服務背景轉送的 audit events，提供集中 Audit Store 與 Audit API。

結帳由 BFF 以伺服器 catalog 計價，再交給 Order service 的持久化 Saga：在同一 SQLite transaction 建立訂單與 checkout 工作，接著預扣庫存及建立付款。明確的業務拒絕會取消訂單並釋放庫存；網路錯誤或 timeout 的結果可能不明，會保留流程並用冪等請求重試。補償失敗也會保存為待重試工作。每個 Go service 會在同一個 SQLite transaction 寫入業務資料與 `audit_outbox`，背景 relay 非同步送到 audit-worker；因此 Audit Worker 或 Audit API 暫時故障時，不會卡住 checkout。SQLite 每個服務獨立持有，正式環境可替換為 PostgreSQL 與 message broker。

## Web／App BFF

PoC 已提供兩個獨立的 Node.js BFF API，兩者共用核心編排邏輯，但依前端型態回傳不同資料格式：

| 用途 | 啟動檔案 | Port | Checkout API | 主要支付回傳 |
| --- | --- | ---: | --- | --- |
| Web／Desktop | `bff-api/src/desktop.js` | `3001` | `POST /api/checkout` | `redirectUrl` |
| App | `bff-api/src/app.js` | `3002` | `POST /api/checkout` | `clientToken` |

對應關係如下：

```text
Next.js Website → Desktop BFF :3001 → Order／Inventory／Payment
Mobile App      → App BFF     :3002 → Order／Inventory／Payment
```

啟動方式：

```bash
cd bff-api && npm run desktop
cd bff-api && PORT=3002 npm run app
```

## Local run

需要 Go 1.22，建議使用 Node.js 22（網站最低要求 Node.js 20.9）。BFF 不依賴第三方 npm 套件；網站使用 Next.js 16 與 React 19，Docker 與網站 CI 採用 Node.js 22，並透過 `package-lock.json`／`npm ci` 固定依賴。

```bash
cd services/order && go mod download && go run .
cd services/inventory && go mod download && go run .
cd services/payment && go mod download && go run .
cd services/audit-worker && go mod download && go run .
cd bff-api && npm run desktop
cd bff-api && PORT=3002 npm run app
cd website && npm ci && NEXT_PUBLIC_BFF_URL=http://localhost:3001 npm run dev
```

也可一次啟動完整環境：

```bash
docker compose up --build
```

網站為 http://localhost:3000，desktop BFF 為 http://localhost:3001，app BFF 為 http://localhost:3002，audit-worker 為 http://localhost:8084。App checkout 範例：

```bash
curl -X POST http://localhost:3002/api/checkout \
  -H 'content-type: application/json' \
  -d '{"user_id":"app-user","items":[{"sku":"mug","quantity":1,"unit_price_cents":650}]}'
```

取得 payment id 後可呼叫 `POST /api/payments/{id}/confirm`，BFF 會同步把訂單狀態更新為 `paid`。

Audit events 由三個 Go service 寫入各自的 `audit_outbox`，背景 relay 送至 audit-worker 的 Audit Store，再由 Node.js BFF 讀取：

```text
Go service audit_outbox → background relay → audit-worker Audit Store
GET http://localhost:3001/api/audit/events
```

Audit Worker API 為 `GET http://localhost:8084/v1/audit/events`；Node.js BFF 的 `GET /api/audit/events` 會轉送 query filters。Next.js audit dashboard 位於 http://localhost:3000/audit。BFF 支援 `service`、`event_type`、`aggregate_id`、`actor_id`、`correlation_id` 與 `limit` query filters，例如 `GET /api/audit/events?service=payment&limit=20`。

## Durable checkout recovery

`POST /api/checkout` accepts an `Idempotency-Key` header (up to 200 characters).
Reuse the same key after a timeout; changing the normalized request with the
same key returns 409. Keys are scoped by Web/App channel. Prices always come
from the BFF catalog; client price fields are ignored.

The successful response keeps the Web/App contract and adds `checkoutId`.
A temporarily unavailable dependency returns HTTP 202 with
`{ "checkoutId": "chk_...", "state": "processing" }`. Poll
`GET /api/checkouts/{checkoutId}`: 202 means pending, 200 means ready, and a
business rejection returns its original error status. Order stores the request,
progress, service results and compensation work in its existing SQLite volume.
Inventory and Payment commit replay responses with business data and audit
outbox entries, so a lost response cannot duplicate a reservation or payment.
Recovery runs every second with bounded backoff; an interrupted claim can be
reclaimed after its 60-second lease expires. Order needs `INVENTORY_API_URL`
and `PAYMENT_API_URL`, configured in Compose and Kubernetes.

BFF upstream requests default to a 3-second deadline, configurable with
`UPSTREAM_TIMEOUT_MS`. If the caller times out before receiving a checkout ID,
retry the original request with its original idempotency key.

## Tests

第一階段測試已加入：三個業務 service 驗證 SQLite transaction 與 `audit_outbox`、audit-worker 驗證 `event_id` 去重、BFF 驗證 Desktop／App response contract 與 Audit API 故障隔離。

本地執行：

```bash
cd services/order && go test ./...
cd services/inventory && go test ./...
cd services/payment && go test ./...
cd services/audit-worker && go test ./...
cd bff-api && npm test
```

`.github/workflows/unit-test.yml` 會在非 `loadtest-*` branch 執行 Go tests、BFF contract tests 與 Next.js production build。現有 Verify Runbook 則保留作為 Docker Compose E2E smoke test。

### Automated loadtest

`.github/workflows/loadtest.yml` 只在 `loadtest-` 開頭的 branch 執行。Push、來源 branch 以 `loadtest-` 開頭的 Pull Request，以及手動觸發都會啟動新 Compose project，等待所有 service healthy 後使用 `grafana/k6:0.54.0` 執行 [loadtest/commerce.js](loadtest/commerce.js)。GitHub Actions 以 `github.head_ref` 判斷 Pull Request 的來源 branch；`loadtest-*` branch 不會執行一般 unit／SIT workflow。

壓測包含：

- catalog read：20 requests／second，30 秒。
- audit read：10 requests／second，30 秒。
- Desktop checkout：5 VUs，每個 3 次，包含付款確認與 inventory commit。
- App checkout：3 VUs，每個 2 次，包含 App payment contract 與 inventory commit。

門檻包含各 checkout scenario 的 checks 全數成功與零 HTTP failure、Desktop／App 完整交易成功率 100% 與固定嘗試數（15／6）；讀取 scenario 的 checks 成功率大於 95%。全域 HTTP failure rate 小於 5%，catalog p95 小於 500ms、audit p95 小於 750ms、兩種 checkout p95 小於 1500ms。Checkout 使用 fresh volumes 與有限 iterations，避免 PoC 的有限 seed inventory 把壓測誤判成大量 `insufficient_stock`。CI 會保存 k6 summary、k6 log、Compose logs 與 service 狀態七天，完成後清理 containers／volumes。

本地執行需要 Docker daemon 可用：

```bash
export COMPOSE_PROJECT_NAME=poc-loadtest-local
docker compose up --build -d
mkdir -p artifacts/loadtest
docker run --rm --network host \
  --volume "$PWD/loadtest:/scripts:ro" \
  --volume "$PWD/artifacts/loadtest:/results" \
  grafana/k6:0.54.0 run --summary-export=/results/k6-summary.json /scripts/commerce.js
docker compose down -v --remove-orphans
```

### Automated SIT

`.github/workflows/sit.yml` 使用真實 Docker Compose containers 與 SQLite，執行 `scripts/sit.sh`。Push 到名稱不以 `loadtest-` 開頭的 branch、來源 branch 不以 `loadtest-` 開頭的 PR，以及在符合條件的 branch 手動觸發時，都會執行。`loadtest-` 開頭的 branch（含 `/` 子路徑）會排除；一般 unit-test CI 與 SIT 是獨立的 checks。

SIT 會驗證：

- 全部 API health、Next.js storefront 與 `/audit` HTTP 回應。
- Desktop／App checkout、付款確認、重複付款拒絕、庫存扣減與 commit。
- 庫存不足時訂單取消；Payment service 停止時回傳處理中，Order 重啟後仍可恢復流程且庫存只扣一次。
- Audit Worker 停止期間 checkout 仍成功；重啟業務服務後，outbox 仍可補送至恢復的 Audit Store。
- 使用唯一 `correlation_id` 等待指定事件送達，確認三個服務的事件與 actor；重複投遞去重與 service／limit 篩選。

需要 Docker Compose v2、Bash、curl、jq，以及可下載 container images／依賴的網路。本地可在專案根目錄執行以下步驟；請使用獨立測試環境並停止占用 3000–3002、8081–8084 的服務：

```bash
export COMPOSE_PROJECT_NAME=poc-sit-local
docker compose up --build -d
bash scripts/sit.sh
docker compose logs --no-color > artifacts/sit/compose.log 2>&1
docker compose down -v --remove-orphans
```

腳本會停止／重啟 Payment、Audit Worker 與業務服務，不適合對共享開發環境執行。每次測試應使用新的 volumes。CI 會自動保存 build log、測試回應、SIT log 與 Compose logs 為 `sit-<run_id>-<run_attempt>` artifact（保留 7 天），並以 `always()` 清理 containers 與測試 volumes，即使測試失敗也會執行。這是 HTTP／資料層的 SIT；未包含瀏覽器操作測試或壓測。

## Verify runbook

以下步驟使用 Docker Compose，會驗證服務健康狀態、Desktop／App BFF checkout、付款確認，以及庫存不足時的 Saga 補償。需要 `curl` 與 `jq`。

### 1. 啟動 PoC

```bash
docker compose up --build -d
docker compose ps
```

等待七個 HTTP container 顯示 `running` 後再繼續。若要查看啟動錯誤：

```bash
docker compose logs --tail=100 audit-worker order inventory payment desktop-bff app-bff website
```

### 2. 驗證 health endpoints

```bash
for url in \
  http://localhost:8081/healthz \
  http://localhost:8082/healthz \
  http://localhost:8083/healthz \
  http://localhost:8084/healthz \
  http://localhost:3001/healthz \
  http://localhost:3002/healthz; do
  curl --fail --silent "$url" | jq .
done

curl --fail --silent --head http://localhost:3000
```

每個 API 應回傳 `{"status":"ok"...}`；website 應回傳 HTTP 200。

### 3. 驗證 Desktop checkout 與付款確認

先記錄庫存，再透過 Desktop BFF 建立訂單：

```bash
before=$(curl --fail --silent http://localhost:8082/v1/stock/mug | jq -r .available)
desktop_response=$(curl --fail --silent --request POST http://localhost:3001/api/checkout \
  --header 'content-type: application/json' \
  --data '{"user_id":"verify-desktop","items":[{"sku":"mug","quantity":1,"unit_price_cents":650}]}')
echo "$desktop_response" | jq .

order_id=$(echo "$desktop_response" | jq -r .order.id)
payment_id=$(echo "$desktop_response" | jq -r .payment.id)
reservation_id=$(echo "$desktop_response" | jq -r .inventory.reservationId)
test "$order_id" != "null" && test "$payment_id" != "null" && test "$reservation_id" != "null"
```

確認付款後，訂單應變成 `paid`，庫存預扣量應減少一件：

```bash
curl --fail --silent --request POST "http://localhost:3001/api/payments/$payment_id/confirm" | jq .
curl --fail --silent "http://localhost:3001/api/orders/$order_id" | jq '{id,status,total_cents}'
after=$(curl --fail --silent http://localhost:8082/v1/stock/mug | jq -r .available)
test "$after" -eq "$((before - 1))"
```

PoC 的 `confirm` 會完成付款與訂單狀態更新；若要把已預扣的 reservation 標記為最終提交，執行：

```bash
curl --fail --silent --request POST "http://localhost:8082/v1/reservations/$reservation_id/commit" | jq .
curl --fail --silent "http://localhost:8082/v1/reservations/$reservation_id" | jq .
```

最後一個回應的 `status` 應為 `committed`。

### 4. 驗證 App BFF 的資料形狀

App BFF 使用相同的核心流程，但回傳 `clientToken` 與 App 專用欄位：

```bash
app_response=$(curl --fail --silent --request POST http://localhost:3002/api/checkout \
  --header 'content-type: application/json' \
  --data '{"user_id":"verify-app","items":[{"sku":"tea-leaves","quantity":1,"unit_price_cents":280}]}')
echo "$app_response" | jq .
test "$(echo "$app_response" | jq -r .state)" = "awaiting_payment"
test "$(echo "$app_response" | jq -r .payment.clientToken)" != "null"
```

### 5. 驗證 Audit API 與 Dashboard

前面的 checkout 與付款步驟會在三個 Go service 的 `audit_outbox` 產生 events，背景 relay 會非同步送到 audit-worker。先等待事件送達，再確認 BFF 能讀取集中 Audit Store：

```bash
for attempt in {1..10}; do
  count=$(curl --fail --silent 'http://localhost:3001/api/audit/events?limit=1' | jq -r .count)
  [ "$count" -gt 0 ] && break
  sleep 1
done
curl --fail --silent 'http://localhost:3001/api/audit/events?limit=20' | jq '{count,services,events: [.events[] | {event_type,service,aggregate_id,status}]}'
curl --fail --silent 'http://localhost:3001/api/audit/events?service=payment' | jq .
```

開啟 http://localhost:3000/audit，應看到依時間排序的 order、inventory、payment events；下拉選單可切換單一 service。

### 6. 驗證 Audit Worker 故障不阻塞 checkout

暫停 Audit Worker，checkout 仍應成功；事件會留在各服務的 `audit_outbox`，Worker 恢復後由 relay 重試送出：

```bash
docker compose stop audit-worker
curl --fail --silent --request POST http://localhost:3001/api/checkout \
  --header 'content-type: application/json' \
  --data '{"user_id":"verify-audit-outage","items":[{"sku":"coffee-beans","quantity":1,"unit_price_cents":420}]}' | jq .
docker compose start audit-worker
```

checkout response 應為 HTTP 201；重新查詢 `GET /api/audit/events` 時，事件可能有數秒的最終一致性延遲。

### 7. 驗證 Saga 補償

請求超過可用庫存，應回傳 HTTP 409。BFF 會取消已建立的訂單；庫存數量不應被扣減：

```bash
before_failure=$(curl --fail --silent http://localhost:8082/v1/stock/mug | jq -r .available)
status=$(curl --silent --output /tmp/checkout-failure.json --write-out '%{http_code}' \
  --request POST http://localhost:3001/api/checkout \
  --header 'content-type: application/json' \
  --data '{"user_id":"verify-failure","items":[{"sku":"mug","quantity":999,"unit_price_cents":650}]}')
cat /tmp/checkout-failure.json | jq .
test "$status" = "409"
after_failure=$(curl --fail --silent http://localhost:8082/v1/stock/mug | jq -r .available)
test "$before_failure" -eq "$after_failure"
```

### 8. 清理環境

```bash
docker compose down
```

若要連同 SQLite volumes 一起重置資料：

```bash
docker compose down -v
```

## Kubernetes

先建置並推送 manifest 中使用的 `poc/*:dev` images（包含 `poc/audit-worker:dev`，或改成 registry image），再執行：

```bash
kubectl apply -f k8s/poc.yaml
```

NodePort：website 30000、desktop BFF 30001、app BFF 30002。audit-worker 僅提供 cluster-internal Service，不直接暴露給瀏覽器。SQLite PVC 只作單副本 PoC；正式環境需要可擴展的資料庫與 secrets／TLS。
