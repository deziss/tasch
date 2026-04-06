#!/bin/bash
set -e

# Test config
TEST_DIR=$(mktemp -d)
MASTER_CONFIG="$TEST_DIR/master.yaml"
WORKER1_CONFIG="$TEST_DIR/worker1.yaml"
WORKER2_CONFIG="$TEST_DIR/worker2.yaml"

TASCH="./bin/tasch"

cleanup() {
    echo "Tearing down..."
    kill $MASTER_PID $WORKER1_PID $WORKER2_PID 2>/dev/null || true
    wait $MASTER_PID $WORKER1_PID $WORKER2_PID 2>/dev/null || true
    rm -rf "$TEST_DIR"
}
trap cleanup EXIT

echo "=== Building ==="
make build

# Write test configs
cat > "$MASTER_CONFIG" <<EOF
role: both
node_name: node-1
master_addr: 127.0.0.1
ports:
  gossip: 7950
  grpc: 50052
  zmq: 5556
EOF

cat > "$WORKER2_CONFIG" <<EOF
role: worker
node_name: node-2
master_addr: 127.0.0.1
ports:
  gossip: 7950
  grpc: 50052
  zmq: 5556
EOF

echo ""
echo "=== Starting master + worker (both mode) ==="
$TASCH start --config="$MASTER_CONFIG" > "$TEST_DIR/master.log" 2>&1 &
MASTER_PID=$!
sleep 2

echo "=== Starting worker 2 ==="
$TASCH start --config="$WORKER2_CONFIG" > "$TEST_DIR/worker2.log" 2>&1 &
WORKER2_PID=$!
# Assign a dummy for cleanup
WORKER1_PID=$$
sleep 3

CLI="$TASCH --config=$MASTER_CONFIG"

echo ""
echo "=== Test 1: Cluster Nodes ==="
$CLI nodes

echo ""
echo "=== Test 2: Submit basic job ==="
$CLI jobs submit "ad.cpu_cores >= 1" "echo 'Hello from Tasch!'" --user=alice
sleep 2

echo ""
echo "=== Test 3: Submit with priority + walltime ==="
$CLI jobs submit "ad.cpu_cores >= 1" "echo 'Priority job'" --priority=1 --user=bob --walltime=30
sleep 2

echo ""
echo "=== Test 4: List all jobs ==="
$CLI jobs

echo ""
echo "=== Test 5: Job detail ==="
FIRST_JOB=$($CLI jobs 2>/dev/null | tail -n +3 | head -1 | awk '{print $1}')
if [ -n "$FIRST_JOB" ]; then
    $CLI jobs status "$FIRST_JOB"
fi

echo ""
echo "=== Test 6: Submit + Cancel ==="
$CLI jobs submit "ad.cpu_cores >= 9999" "echo 'never'" --user=test
sleep 1
CANCEL_JOB=$($CLI jobs --state=QUEUED 2>/dev/null | tail -n +3 | head -1 | awk '{print $1}')
if [ -n "$CANCEL_JOB" ]; then
    echo "Cancelling job $CANCEL_JOB..."
    $CLI jobs cancel "$CANCEL_JOB"
fi

echo ""
echo "=== Test 7: Stream logs ==="
LOG_JOB=$($CLI jobs 2>/dev/null | tail -n +3 | head -1 | awk '{print $1}')
if [ -n "$LOG_JOB" ]; then
    timeout 2 $CLI jobs logs "$LOG_JOB" 2>/dev/null || true
fi

echo ""
echo "=== Test 8: Distributed training ==="
$CLI jobs train "echo 'Rank=\$RANK WorldSize=\$WORLD_SIZE MasterAddr=\$MASTER_ADDR'" \
  --nodes=2 --gpus-per-node=0 --requirement="ad.cpu_cores >= 1" --user=trainer
sleep 4
echo "Group job listing:"
$CLI jobs 2>/dev/null | grep "dj-" || echo "(no group jobs found)"

echo ""
echo "=== Test 9: Env var passing ==="
$CLI jobs submit "ad.cpu_cores >= 1" "echo MY_VAR=\$MY_VAR" --env="MY_VAR=hello_gpu" --user=envtest
sleep 2
ENV_JOB=$($CLI jobs 2>/dev/null | grep "envtest" | head -1 | awk '{print $1}')
if [ -n "$ENV_JOB" ]; then
    $CLI jobs status "$ENV_JOB"
fi

echo ""
echo "=== Test 10: tasch nodes (from worker config) ==="
$TASCH nodes --config="$WORKER2_CONFIG"

echo ""
echo "=== Test 11: tasch jobs (from worker config) ==="
$TASCH jobs --config="$WORKER2_CONFIG" | head -5

echo ""
echo "=== All tests complete! ==="
echo ""
echo "--- Master/Worker-1 Log ---"
cat "$TEST_DIR/master.log"
echo ""
echo "--- Worker 2 Log ---"
cat "$TEST_DIR/worker2.log"
