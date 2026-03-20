#!/usr/bin/env bash
#
# E2E test for the LLM spending proxy — pure CLI, no Go code.
# Exercises: ddld daemon, ddl CLI, curl through the proxy, mock LLM upstream.
#
# Requirements: Docker (running), python3, curl, jq, Go toolchain
#
# Usage:
#   ./e2e/cli_proxy_test.sh
#
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# ── Colours (if tty) ────────────────────────────────────────────────────────
if [ -t 1 ]; then
    GREEN='\033[0;32m'; RED='\033[0;31m'; BOLD='\033[1m'; RESET='\033[0m'
else
    GREEN=''; RED=''; BOLD=''; RESET=''
fi

# ── Prerequisites ───────────────────────────────────────────────────────────
for cmd in docker python3 curl jq go; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "SKIP: $cmd not found"
        exit 0
    fi
done

if ! docker info &>/dev/null 2>&1; then
    echo "SKIP: Docker is not running"
    exit 0
fi

# ── Build binaries ──────────────────────────────────────────────────────────
echo "${BOLD}Building binaries...${RESET}"
TMPBIN=$(mktemp -d)
go build -o "$TMPBIN/ddld" "$PROJECT_DIR/cmd/ddld" 2>&1
go build -o "$TMPBIN/ddl"  "$PROJECT_DIR/cmd/ddl"  2>&1
echo "  ddld -> $TMPBIN/ddld"
echo "  ddl  -> $TMPBIN/ddl"

# ── Free port helper ────────────────────────────────────────────────────────
free_port() {
    python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()'
}

API_PORT=$(free_port)
MOCK_PORT=$(free_port)
TMPDIR=$(mktemp -d)
CONTAINER_NAME="ddl-e2e-proxy-$$"

# ── Cleanup on exit ─────────────────────────────────────────────────────────
cleanup() {
    echo ""
    echo "${BOLD}Cleaning up...${RESET}"
    [ -n "${DDLD_PID:-}" ] && kill "$DDLD_PID" 2>/dev/null; wait "$DDLD_PID" 2>/dev/null || true
    [ -n "${MOCK_PID:-}" ] && kill "$MOCK_PID" 2>/dev/null; wait "$MOCK_PID" 2>/dev/null || true
    docker rm -f "$CONTAINER_NAME" &>/dev/null || true
    rm -rf "$TMPBIN" "$TMPDIR"
}
trap cleanup EXIT

# ── Test framework ──────────────────────────────────────────────────────────
TESTS=0; PASS=0; FAIL=0

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    TESTS=$((TESTS+1))
    if printf '%s' "$haystack" | grep -qF "$needle"; then
        PASS=$((PASS+1)); echo -e "  ${GREEN}PASS${RESET}: $desc"
    else
        FAIL=$((FAIL+1)); echo -e "  ${RED}FAIL${RESET}: $desc"
        echo "    expected to contain: $needle"
        echo "    got: $haystack"
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    TESTS=$((TESTS+1))
    if ! printf '%s' "$haystack" | grep -qF "$needle"; then
        PASS=$((PASS+1)); echo -e "  ${GREEN}PASS${RESET}: $desc"
    else
        FAIL=$((FAIL+1)); echo -e "  ${RED}FAIL${RESET}: $desc"
        echo "    expected NOT to contain: $needle"
        echo "    got: $haystack"
    fi
}

assert_http() {
    local desc="$1" expected="$2" actual="$3"
    TESTS=$((TESTS+1))
    if [ "$expected" = "$actual" ]; then
        PASS=$((PASS+1)); echo -e "  ${GREEN}PASS${RESET}: $desc (HTTP $actual)"
    else
        FAIL=$((FAIL+1)); echo -e "  ${RED}FAIL${RESET}: $desc (expected HTTP $expected, got HTTP $actual)"
    fi
}

assert_gt() {
    local desc="$1" val="$2" threshold="$3"
    TESTS=$((TESTS+1))
    if [ "$val" -gt "$threshold" ] 2>/dev/null; then
        PASS=$((PASS+1)); echo -e "  ${GREEN}PASS${RESET}: $desc ($val > $threshold)"
    else
        FAIL=$((FAIL+1)); echo -e "  ${RED}FAIL${RESET}: $desc (expected > $threshold, got $val)"
    fi
}

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    TESTS=$((TESTS+1))
    if [ "$expected" = "$actual" ]; then
        PASS=$((PASS+1)); echo -e "  ${GREEN}PASS${RESET}: $desc"
    else
        FAIL=$((FAIL+1)); echo -e "  ${RED}FAIL${RESET}: $desc (expected '$expected', got '$actual')"
    fi
}

# ── CLI wrapper ─────────────────────────────────────────────────────────────
ddl() {
    "$TMPBIN/ddl" --api "http://127.0.0.1:$API_PORT" "$@"
}

# Helper: curl through the proxy to the mock OpenAI endpoint.
# Sets CURL_STATUS and CURL_BODY.
proxy_curl() {
    local full
    full=$(curl -s -w "\n%{http_code}" \
        -x "http://$PROXY_ADDR" \
        -H "Content-Type: application/json" \
        -d '{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}' \
        "http://api.openai.com:$MOCK_PORT/v1/chat/completions" 2>&1)
    CURL_STATUS=$(echo "$full" | tail -1)
    CURL_BODY=$(echo "$full" | sed '$d')
}

# Helper: get spending milli-cents from the API (JSON, precise).
get_spending() {
    curl -s "http://127.0.0.1:$API_PORT/containers/$1/usage" | jq -r '.spending // 0'
}

# ── Start mock LLM server ──────────────────────────────────────────────────
# Returns gpt-4 pricing: 10 000 prompt + 5 000 completion tokens per call.
# gpt-4: 3000 / 6000 micro-cents per token → 60,000 milli-cents per call.
echo ""
echo "${BOLD}Starting mock LLM API on port $MOCK_PORT...${RESET}"
python3 - "$MOCK_PORT" <<'PYEOF' &
import json, sys
from http.server import HTTPServer, BaseHTTPRequestHandler

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        # Must read the request body to avoid connection resets
        length = int(self.headers.get("Content-Length", 0))
        if length > 0:
            self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({
            "id": "chatcmpl-test",
            "model": "gpt-4",
            "usage": {"prompt_tokens": 10000, "completion_tokens": 5000},
            "choices": [{"message": {"role": "assistant", "content": "hello"}}]
        }).encode())
    def log_message(self, *_): pass

HTTPServer(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
PYEOF
MOCK_PID=$!

# Wait for mock server to accept connections
for i in $(seq 1 30); do
    if curl -sf -X POST -H "Content-Type: application/json" -d '{}' \
        "http://127.0.0.1:$MOCK_PORT/" >/dev/null 2>&1; then
        echo "  mock LLM API ready"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "FATAL: mock LLM server did not start"
        exit 1
    fi
    sleep 0.1
done

# ── Start ddld ──────────────────────────────────────────────────────────────
echo "${BOLD}Starting ddld on port $API_PORT...${RESET}"
DDL_PROXY_RESOLVE="api.openai.com=127.0.0.1,api.anthropic.com=127.0.0.1" \
    "$TMPBIN/ddld" -addr ":$API_PORT" -db "$TMPDIR/ddl.db" -sock "" \
    >"$TMPDIR/ddld.stdout" 2>"$TMPDIR/ddld.stderr" &
DDLD_PID=$!

echo "  Waiting for health..."
for i in $(seq 1 50); do
    if curl -sf "http://127.0.0.1:$API_PORT/containers" >/dev/null 2>&1; then
        echo "  ddld healthy"
        break
    fi
    if [ "$i" -eq 50 ]; then
        echo "FATAL: ddld did not become healthy"
        cat "$TMPDIR/ddld.stderr"
        exit 1
    fi
    sleep 0.1
done

# ── Create a Docker container ──────────────────────────────────────────────
echo ""
echo "${BOLD}Creating Docker container '$CONTAINER_NAME'...${RESET}"
DOCKER_ID=$(docker run -d --name "$CONTAINER_NAME" alpine sleep 3600)
echo "  Docker ID: $DOCKER_ID"

# ════════════════════════════════════════════════════════════════════════════
# TESTS
# ════════════════════════════════════════════════════════════════════════════

# ── 1. Register container via CLI ───────────────────────────────────────────
echo ""
echo "${BOLD}Test: ddl register${RESET}"
REG_OUT=$(ddl register "$DOCKER_ID" 2>&1)
assert_contains "register prints container ID" "Registered container:" "$REG_OUT"
SHORT_ID=$(echo "$REG_OUT" | head -1 | awk '{print $NF}')
echo "  Short ID: $SHORT_ID"

# Extract proxy address from ddld stderr
sleep 0.3
PROXY_LINE=$(grep "proxy for $SHORT_ID available at" "$TMPDIR/ddld.stderr" | tail -1)
PROXY_ADDR_RAW=$(echo "$PROXY_LINE" | sed 's/.*available at //; s/ .*//')
PROXY_PORT=$(echo "$PROXY_ADDR_RAW" | sed 's/.*://')
PROXY_ADDR="127.0.0.1:$PROXY_PORT"
echo "  Proxy: $PROXY_ADDR"

if [ -z "$PROXY_PORT" ]; then
    echo "FATAL: could not determine proxy address"
    cat "$TMPDIR/ddld.stderr"
    exit 1
fi

# ── 2. List containers via CLI ──────────────────────────────────────────────
echo ""
echo "${BOLD}Test: ddl ls${RESET}"
LS_OUT=$(ddl ls 2>&1)
assert_contains "ls shows container ID"   "$SHORT_ID"       "$LS_OUT"
assert_contains "ls shows container name" "$CONTAINER_NAME"  "$LS_OUT"

# ── 3. Set spending limit via CLI ───────────────────────────────────────────
echo ""
echo "${BOLD}Test: ddl limits set spending${RESET}"
SET_OUT=$(ddl limits set "$SHORT_ID" spending 1.00 2>&1)
assert_contains "set output confirms value" '$1.00' "$SET_OUT"

# ── 4. Verify limit via CLI ────────────────────────────────────────────────
echo ""
echo "${BOLD}Test: ddl limits get${RESET}"
LIM_OUT=$(ddl limits get "$SHORT_ID" 2>&1)
assert_contains "limits get shows spending" '$1.00' "$LIM_OUT"

# ── 5. Proxy forwards request to mock API ──────────────────────────────────
echo ""
echo "${BOLD}Test: proxy forwards request (1st call, ~60,000 milli-cents)${RESET}"
proxy_curl
assert_http   "1st request succeeds"       "200" "$CURL_STATUS"
assert_contains "response body has model"  "gpt-4" "$CURL_BODY"

# ── 6. Spending is tracked (visible via API + CLI) ─────────────────────────
echo ""
echo "${BOLD}Test: spending tracked after 1 request${RESET}"
SPENDING=$(get_spending "$SHORT_ID")
assert_gt "spending > 0 via API" "$SPENDING" 0
echo "  spending = $SPENDING milli-cents"

USAGE_OUT=$(ddl usage "$SHORT_ID" 2>&1)
assert_contains "ddl usage shows dollar amount" '$' "$USAGE_OUT"

# ── 7. Second request still under budget ────────────────────────────────────
echo ""
echo "${BOLD}Test: 2nd request still under budget (~120,000 milli-cents total)${RESET}"
proxy_curl
assert_http "2nd request succeeds" "200" "$CURL_STATUS"

SPENDING=$(get_spending "$SHORT_ID")
assert_gt "spending > 60000 after 2 calls" "$SPENDING" 60000
echo "  spending = $SPENDING milli-cents"

# ── 8. Budget exceeded → 429 ───────────────────────────────────────────────
echo ""
echo "${BOLD}Test: 3rd request blocked (budget exceeded)${RESET}"
proxy_curl
assert_http     "3rd request returns 429"          "429" "$CURL_STATUS"
assert_contains "body says budget exceeded"        "spending budget exceeded" "$CURL_BODY"

# Spending must not increase (request was blocked before proxying)
SPENDING_AFTER_BLOCK=$(get_spending "$SHORT_ID")
assert_eq "spending unchanged after block" "$SPENDING" "$SPENDING_AFTER_BLOCK"

# ── 9. Increase budget via CLI → unblocks ──────────────────────────────────
echo ""
echo "${BOLD}Test: ddl limits increase unblocks proxy${RESET}"
INCR_OUT=$(ddl limits increase "$SHORT_ID" spending 5.00 2>&1)
assert_contains "increase output says increased" "increased" "$INCR_OUT"

proxy_curl
assert_http "request after increase succeeds" "200" "$CURL_STATUS"

# ── 10. Decrease budget via CLI → re-blocks ─────────────────────────────────
echo ""
echo "${BOLD}Test: ddl limits set (low) re-blocks proxy${RESET}"
ddl limits set "$SHORT_ID" spending 0.10 >/dev/null 2>&1   # $0.10 = 10,000 milli-cents
proxy_curl
assert_http "request after tight budget is 429" "429" "$CURL_STATUS"

# ── 11. Usage shows ENFORCED status ────────────────────────────────────────
echo ""
echo "${BOLD}Test: ddl usage shows ENFORCED for spending${RESET}"
# Give the enforcement loop a moment to pick up the exceeded state
sleep 1.5
USAGE_OUT=$(ddl usage "$SHORT_ID" 2>&1)
echo "$USAGE_OUT"
assert_contains "usage output shows ENFORCED" "ENFORCED" "$USAGE_OUT"

# ── 12. Remove container via CLI ────────────────────────────────────────────
echo ""
echo "${BOLD}Test: ddl remove${RESET}"
RM_OUT=$(ddl remove "$SHORT_ID" 2>&1)
assert_contains "remove confirms removal" "removed" "$RM_OUT"

LS_AFTER=$(ddl ls 2>&1)
assert_not_contains "ls no longer shows removed container" "$SHORT_ID" "$LS_AFTER"

# ════════════════════════════════════════════════════════════════════════════
# SUMMARY
# ════════════════════════════════════════════════════════════════════════════
echo ""
echo "=========================================="
echo -e "  ${GREEN}Passed${RESET}: $PASS"
echo -e "  ${RED}Failed${RESET}: $FAIL"
echo -e "  Total:  $TESTS"
echo "=========================================="
[ "$FAIL" -eq 0 ]
