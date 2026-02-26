#!/usr/bin/env bash
#
# llm-budget-demo.sh — Demonstrates spending budget enforcement with the Anthropic API.
#
# Usage:
#   ANTHROPIC_API_KEY=sk-ant-... bash examples/llm-budget-demo.sh
#
# Pass BUILD=1 to force Docker image rebuild (only needed after Go code changes):
#   BUILD=1 ANTHROPIC_API_KEY=sk-ant-... bash examples/llm-budget-demo.sh
#
set -euo pipefail

# --- Config ---
BUDGET_MCENTS=50  # 50 milli-cents = 0.05 cents = $0.0005 — tight enough to trigger with haiku
CONTAINER_NAME="llm-budget-demo"
DDL_PORT=7123

# Helper: call the management API via unix socket inside the daemon container.
ddl_api() {
  docker exec ddl-daemon curl -sf --unix-socket /run/ddl/ddl.sock "$@"
}

# Format milli-cents as dollars
format_mcents() {
  local mc=$1
  python3 -c "print(f'\${$mc/100000:.4f}')"
}

# --- Preflight ---
if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "ERROR: ANTHROPIC_API_KEY env var is required"
  echo "Usage: ANTHROPIC_API_KEY=sk-ant-... bash $0"
  exit 1
fi

cleanup() {
  echo ""
  echo "=== Cleanup ==="
  docker rm -f "$CONTAINER_NAME" 2>/dev/null || true
  ./ddl daemon stop 2>/dev/null || true
  echo "Done."
}
trap cleanup EXIT

echo "=== LLM Budget Demo ==="
echo "Budget: $(format_mcents $BUDGET_MCENTS) ($BUDGET_MCENTS milli-cents)"
echo ""

# --- Step 1: Build CLI + daemon image, start daemon ---
echo "--- Step 1: Building CLI and starting daemon ---"

# Always rebuild the host CLI (fast, ~1s)
echo "Building ddl CLI..."
go build -o ddl ./cmd/ddl

# Build Docker image only if requested or missing
BUILD_FLAG=""
if [[ "${BUILD:-}" == "1" ]] || ! docker image inspect ddld:latest >/dev/null 2>&1; then
  BUILD_FLAG="--build"
  echo "Building ddld Docker image..."
fi

export DDL_ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY"
./ddl daemon start $BUILD_FLAG
sleep 1

# Verify daemon is ready
echo -n "Waiting for daemon API... "
for attempt in $(seq 1 10); do
  if ddl_api "http://localhost/containers" >/dev/null 2>&1; then
    echo "ready."
    break
  fi
  if [[ $attempt -eq 10 ]]; then
    echo "FAILED"
    docker logs ddl-daemon 2>&1 | tail -20
    exit 1
  fi
  sleep 1
done

# Verify API key is configured
if docker logs ddl-daemon 2>&1 | grep -q "Anthropic API key configured"; then
  echo "API key: configured in daemon"
else
  echo "WARNING: API key NOT configured in daemon!"
  echo "Daemon logs:"
  docker logs ddl-daemon 2>&1 | tail -10
  exit 1
fi
echo ""

# --- Step 2: Start a container with curl ---
echo "--- Step 2: Starting demo container ---"
docker run -d --name "$CONTAINER_NAME" alpine:latest sleep 3600
docker exec "$CONTAINER_NAME" apk add --no-cache curl jq >/dev/null 2>&1
CONTAINER_ID=$(docker inspect --format '{{.Id}}' "$CONTAINER_NAME")
echo "Container: $CONTAINER_NAME (${CONTAINER_ID:0:12})"
echo ""

# --- Step 3: Register with ddl ---
echo "--- Step 3: Registering container ---"
REG_RESPONSE=$(ddl_api -X POST "http://localhost/register" \
  -H "Content-Type: application/json" \
  -d "{\"container_id\": \"${CONTAINER_ID}\"}")
DDL_ID=$(echo "$REG_RESPONSE" | jq -r '.id')
RAW_PROXY_ADDR=$(echo "$REG_RESPONSE" | jq -r '.proxy_addr')

# Rewrite proxy address: daemon binds on 0.0.0.0 but demo container reaches via bridge IP
DAEMON_IP=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' ddl-daemon)
PROXY_PORT=${RAW_PROXY_ADDR##*:}
PROXY_ADDR="${DAEMON_IP}:${PROXY_PORT}"
echo "DDL ID: $DDL_ID   Proxy: $PROXY_ADDR"
echo ""

# --- Step 4: Set spending budget ---
echo "--- Step 4: Setting spending budget to $(format_mcents $BUDGET_MCENTS) ($BUDGET_MCENTS milli-cents) ---"
ddl_api -X PUT "http://localhost/containers/${DDL_ID}/limits" \
  -H "Content-Type: application/json" \
  -d "{\"type\": \"spending\", \"value\": ${BUDGET_MCENTS}, \"operation\": \"set\"}" | jq .
echo ""

# --- Step 5: Make API calls through the proxy ---
echo "--- Step 5: Making API calls through proxy ---"
echo "(Container sends plain HTTP to api.anthropic.com via proxy."
echo " Proxy upgrades to HTTPS and injects the real API key.)"
echo ""

PROMPTS=(
  "What is the tallest mountain on Earth? Answer in one sentence."
  "Name three primary colors."
  "Write a haiku about the ocean."
  "What year did humans first land on the Moon? One sentence."
  "Explain gravity to a 5-year-old in two sentences."
  "What is the speed of light in km/s? One sentence."
  "Name the four seasons."
  "Who painted the Mona Lisa? One sentence."
  "What is the chemical formula for water?"
  "Say goodbye in three different languages."
)

for i in $(seq 1 10); do
  PROMPT="${PROMPTS[$((i-1))]}"
  REQUEST_BODY=$(jq -n \
    --arg prompt "$PROMPT" \
    '{model:"claude-haiku-4-5-20251001",max_tokens:100,messages:[{role:"user",content:$prompt}]}')

  echo "Request #$i: \"$PROMPT\""

  HTTP_CODE=$(docker exec "$CONTAINER_NAME" \
    curl -s -o /tmp/resp.json -w '%{http_code}' \
    -X POST "http://api.anthropic.com/v1/messages" \
    -H "Content-Type: application/json" \
    -d "$REQUEST_BODY" \
    --proxy "http://${PROXY_ADDR}" 2>/dev/null) || HTTP_CODE="000"

  if [[ "$HTTP_CODE" == "429" ]]; then
    echo "  => HTTP 429 — Budget exceeded!"
    docker exec "$CONTAINER_NAME" cat /tmp/resp.json 2>/dev/null || true
    echo ""
    break
  elif [[ "$HTTP_CODE" == "200" ]]; then
    RESPONSE=$(docker exec "$CONTAINER_NAME" cat /tmp/resp.json 2>/dev/null)
    MODEL=$(echo "$RESPONSE" | jq -r '.model // "?"')
    INPUT_TOK=$(echo "$RESPONSE" | jq -r '.usage.input_tokens // 0')
    OUTPUT_TOK=$(echo "$RESPONSE" | jq -r '.usage.output_tokens // 0')
    TEXT=$(echo "$RESPONSE" | jq -r '.content[0].text // "(no text)"')
    echo "  => $TEXT"
    echo "     [model=$MODEL  tokens: $INPUT_TOK in / $OUTPUT_TOK out]"
  else
    echo "  => HTTP $HTTP_CODE"
    docker exec "$CONTAINER_NAME" cat /tmp/resp.json 2>/dev/null || true
    echo ""
    # Show daemon logs on first failure for debugging
    if [[ "$i" == "1" ]]; then
      echo "  [daemon logs]"
      docker logs ddl-daemon 2>&1 | tail -5
      echo ""
    fi
  fi

  # Show current spending
  SPENDING=$(ddl_api "http://localhost/containers/${DDL_ID}/usage" | jq -r '.spending // 0')
  echo "     Spending: $(format_mcents $SPENDING) / $(format_mcents $BUDGET_MCENTS)  ($SPENDING / $BUDGET_MCENTS milli-cents)"
  echo ""

  sleep 1
done

# --- Step 6: Final status ---
echo "--- Final Status ---"
ddl_api "http://localhost/containers/${DDL_ID}" | jq .
echo ""
echo "=== Demo complete ==="
