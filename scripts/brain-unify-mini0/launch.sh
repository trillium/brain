#!/bin/sh
# Launch the unify run detached on mini0 (so a dropped ssh cannot kill it) and
# stay attached only as a watcher, so one terminal notification covers the
# whole build+verify.
set -u
cd /Users/mini0/brain-unify || exit 1
nohup sh run-unify.sh >logs/runner.out 2>&1 </dev/null &
pid=$!
echo "launched runner pid=$pid at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
while kill -0 "$pid" 2>/dev/null; do sleep 30; done
echo "=== runner exited $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
echo "--- run.log ---"
cat logs/run.log 2>/dev/null
echo "--- build.exit / verify.exit ---"
cat logs/build.exit 2>/dev/null
cat logs/verify.exit 2>/dev/null
echo "--- build.log tail ---"
tail -25 logs/build.log 2>/dev/null
echo "--- verify.log tail ---"
tail -40 logs/verify.log 2>/dev/null
