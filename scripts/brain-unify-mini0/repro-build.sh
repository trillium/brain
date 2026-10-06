#!/bin/sh
# Reproducibility test for the metadata defect: rebuild from the same frozen
# source into a SEPARATE data dir, then ask whether the same two beads lose
# their metadata JSON again. Deterministic => a real, reportable defect;
# different rows => nondeterministic.
set -u
BASE=/Users/mini0/brain-unify
BIN=$BASE/bin/bd
LOGS=$BASE/logs
SCRATCH=$BASE/scratch/repro
M=/opt/homebrew/opt/mysql-client/bin/mysql
PORT=3394

rm -rf "$SCRATCH"
mkdir -p "$SCRATCH"
echo "repro build started $(date -u +%Y-%m-%dT%H:%M:%SZ)"
"$BIN" brain unify build --host 127.0.0.1 --port 3391 \
	--data-dir "$SCRATCH" --allow-collisions --timeout 4h >"$LOGS/repro-build.log" 2>&1
st=$?
echo "repro build exit=$st elapsed=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
[ "$st" = "0" ] || exit "$st"

nohup /opt/homebrew/bin/dolt sql-server -H 127.0.0.1 -P "$PORT" --loglevel=error \
	--data-dir "$SCRATCH" >/tmp/repro-srv.log 2>&1 &
i=0
while [ "$i" -lt 120 ]; do nc -z 127.0.0.1 "$PORT" 2>/dev/null && break; i=$((i + 1)); sleep 1; done

echo "=== the two beads from the first build, in the REBUILT database ==="
$M -h 127.0.0.1 -P "$PORT" -u root -B -e \
	"select id, length(metadata) from brain_unified.issues where id in ('task-a44d4','task-ybur')" 2>/dev/null

echo "=== how many tasks-namespace rows have metadata of length 2 ('{}') in the rebuild? ==="
$M -h 127.0.0.1 -P "$PORT" -u root -N -B -e \
	"select count(*) from brain_unified.issues where substring_index(id,'-',1)='task' and length(metadata)=2" 2>/dev/null

echo "=== same figure in the FIRST (delivered) build ==="
ssh -o BatchMode=yes localhost "M=$M; $M -h 127.0.0.1 -P 3392 -u root -N -B -e \"select count(*) from brain_unified.issues where substring_index(id,'-',1)='task' and length(metadata)=2\"" 2>/dev/null ||
	$M -h 127.0.0.1 -P 3392 -u root -N -B -e \
		"select count(*) from brain_unified.issues where substring_index(id,'-',1)='task' and length(metadata)=2" 2>/dev/null

echo "repro done $(date -u +%Y-%m-%dT%H:%M:%SZ)"
