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
    rm -f ~/.tasch/tasch.db ~/.tasch/tasch.pid  # Clean test state
}
trap cleanup EXIT

# Clean state from previous runs
rm -f ~/.tasch/tasch.db ~/.tasch/tasch.pid

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
  metrics: 9092
EOF

cat > "$WORKER2_CONFIG" <<EOF
role: worker
node_name: node-2
master_addr: 127.0.0.1
ports:
  gossip: 7950
  grpc: 50052
  zmq: 5556
  metrics: 9092
EOF

echo ""
echo "=== Starting master + worker (both mode) ==="
$TASCH start --config="$MASTER_CONFIG" > "$TEST_DIR/master.log" 2>&1 &
MASTER_PID=$!
sleep 2

echo "=== Starting worker 2 ==="
mkdir -p "$TEST_DIR/home2"
env HOME="$TEST_DIR/home2" $TASCH start --config="$WORKER2_CONFIG" > "$TEST_DIR/worker2.log" 2>&1 &
WORKER2_PID=$!
# Assign a dummy for cleanup
WORKER1_PID=$$
sleep 3

CLI="$TASCH --config=$MASTER_CONFIG"

# Helper function to wait for a job to reach a specific state
wait_for_job() {
    local job_id=$1
    local expected=$2
    local timeout_secs=$3
    local elapsed=0
    while [ $elapsed -lt $timeout_secs ]; do
        local state=$($CLI jobs 2>/dev/null | grep "$job_id" | awk '{print $2}')
        if [ "$state" = "$expected" ]; then
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    echo "ERROR: Job $job_id did not reach state $expected within ${timeout_secs}s (current state: $($CLI jobs 2>/dev/null | grep "$job_id" | awk '{print $2}'))"
    exit 1
}

echo ""
echo "=== Test 1: Cluster Nodes ==="
NODES_OUT=$($CLI nodes)
echo "$NODES_OUT"
if ! echo "$NODES_OUT" | grep -q "node-1" || ! echo "$NODES_OUT" | grep -q "node-2"; then
    echo "ERROR: Both node-1 and node-2 must be active in the cluster!"
    echo "--- MASTER LOG ---"
    cat "$TEST_DIR/master.log"
    echo "--- WORKER2 LOG ---"
    cat "$TEST_DIR/worker2.log"
    exit 1
fi

echo ""
echo "=== Test 2: Submit basic job ==="
JOB_ID=$($CLI jobs submit "ad.cpu_cores >= 1" "echo 'Hello from Tasch!'" --user=alice | grep "ID:" | awk '{print $2}')
echo "Submitted job ID: $JOB_ID"
wait_for_job "$JOB_ID" "COMPLETED" 10

echo ""
echo "=== Test 3: Submit with priority + walltime ==="
JOB_ID=$($CLI jobs submit "ad.cpu_cores >= 1" "echo 'Priority job'" --priority=1 --user=bob --walltime=30 | grep "ID:" | awk '{print $2}')
echo "Submitted job ID: $JOB_ID"
wait_for_job "$JOB_ID" "COMPLETED" 10

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
JOB_ID=$($CLI jobs submit "ad.cpu_cores >= 9999" "echo 'never'" --user=test | grep "ID:" | awk '{print $2}')
echo "Submitted job ID: $JOB_ID (waiting in queue)"
sleep 1
echo "Cancelling job $JOB_ID..."
$CLI jobs cancel "$JOB_ID"
wait_for_job "$JOB_ID" "CANCELLED" 5

echo ""
echo "=== Test 7: Stream logs ==="
LOG_JOB=$($CLI jobs 2>/dev/null | grep "COMPLETED" | head -1 | awk '{print $1}')
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
JOB_ID=$($CLI jobs submit "ad.cpu_cores >= 1" "echo MY_VAR=\$MY_VAR" --env="MY_VAR=hello_gpu" --user=envtest | grep "ID:" | awk '{print $2}')
echo "Submitted job ID: $JOB_ID"
wait_for_job "$JOB_ID" "COMPLETED" 10
$CLI jobs status "$JOB_ID"

echo ""
echo "=== Test 10: tasch nodes (from worker config) ==="
$TASCH nodes --config="$WORKER2_CONFIG"

echo ""
echo "=== Test 11: tasch jobs (from worker config) ==="
$TASCH jobs --config="$WORKER2_CONFIG" | head -5

echo ""
echo "=== Test 12: Multi-resource constraint (CPUs & Memory) ==="
JOB_ID=$($CLI jobs submit "ad.cpu_cores >= 1 && ad.total_memory_mb >= 512" "echo 'Multi-resource constraints met!'" --user=alice | grep "ID:" | awk '{print $2}')
echo "Submitted job ID: $JOB_ID"
wait_for_job "$JOB_ID" "COMPLETED" 10

echo ""
echo "=== Test 13: Exceed resource constraints (should remain QUEUED) ==="
JOB_ID=$($CLI jobs submit "ad.total_memory_mb >= 999999" "echo 'This should not run'" --user=greedy | grep "ID:" | awk '{print $2}')
echo "Submitted job ID: $JOB_ID"
sleep 2
STATE=$($CLI jobs 2>/dev/null | grep "$JOB_ID" | awk '{print $2}')
if [ "$STATE" != "QUEUED" ]; then
    echo "ERROR: Greedy job was dispatched unexpectedly! Current state: $STATE"
    exit 1
fi
echo "Greedy job correctly remains in state: $STATE"
echo "Cancelling greedy job..."
$CLI jobs cancel "$JOB_ID"
wait_for_job "$JOB_ID" "CANCELLED" 5

echo ""
echo "=== All tests complete! ==="
echo ""
echo "--- Master/Worker-1 Log ---"
cat "$TEST_DIR/master.log"
echo ""
echo "--- Worker 2 Log ---"
cat "$TEST_DIR/worker2.log"
