#!/bin/sh
set -e

# Use the DinD wrapper to set up cgroup v2 nesting, mount propagation, etc.
# Then start dockerd in the background.
echo "=== Initializing DinD environment ==="
/usr/local/bin/dind dockerd --storage-driver=overlay2 &

# Wait for Docker daemon to be ready
echo "=== Waiting for Docker daemon ==="
timeout=30
elapsed=0
while ! docker info >/dev/null 2>&1; do
    sleep 1
    elapsed=$((elapsed + 1))
    if [ "$elapsed" -ge "$timeout" ]; then
        echo "ERROR: Docker daemon did not start within ${timeout}s"
        exit 1
    fi
done
echo "=== Docker daemon ready (${elapsed}s) ==="

echo "=== Pulling alpine:latest ==="
docker pull alpine:latest

echo "=== Running E2E tests ==="
/usr/local/bin/e2e.test -test.v -test.count=1 -test.timeout=120s
exit_code=$?

echo "=== E2E tests finished with exit code ${exit_code} ==="
exit $exit_code
