#!/bin/sh
# Run `bd brain unify build` then `verify` ON mini0, against a frozen copy of
# the production federation, keeping per-table/per-source timestamped progress
# and a memory trace alongside.
#
# Nothing here writes to production: the source is a private copy of the
# macbook's Dolt data directory served on loopback, and the build's only
# writable connection is the isolated Dolt server it starts under --data-dir.
#
# The source server is started once and left up across both phases so that
# build and verify read byte-identical data; on the macbook a live source moves
# under the build (the lifespan ledger alone takes hundreds of rows an hour),
# which would make a verification failure ambiguous.
set -u

BASE=/Users/mini0/brain-unify
BIN=$BASE/bin/bd
SRC=$BASE/prod-src/.beads
PORT=${SRC_PORT:-3391}
LOGS=$BASE/logs
SCRATCH=$BASE/scratch/unified
DOLT=${DOLT_BIN:-/opt/homebrew/bin/dolt}

rm -rf "$SCRATCH"
mkdir -p "$SCRATCH" "$LOGS"
: >"$LOGS/mem.csv"

# --- source server over the frozen copy -------------------------------------
if nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
	echo "=== reusing source server already on port $PORT ===" | tee -a "$LOGS/run.log"
else
	echo "=== starting source server on port $PORT over $SRC ===" | tee -a "$LOGS/run.log"
	"$DOLT" sql-server -H 127.0.0.1 -P "$PORT" --loglevel=error --data-dir "$SRC" \
		>"$LOGS/source-server.log" 2>&1 &
	echo $! >"$LOGS/source-server.pid"
	i=0
	while [ "$i" -lt 180 ]; do
		nc -z 127.0.0.1 "$PORT" 2>/dev/null && break
		i=$((i + 1))
		sleep 1
	done
	if ! nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
		echo "=== source server did not come up in 180s; see $LOGS/source-server.log ===" | tee -a "$LOGS/run.log"
		exit 1
	fi
	echo "=== source server up $(date -u +%Y-%m-%dT%H:%M:%SZ) ===" | tee -a "$LOGS/run.log"
fi

# --- memory / swap trace -----------------------------------------------------
# Sampled every 5s so a rise toward the earlier 10GB RSS is visible while it
# happens rather than after the run is killed.
sample() {
	printf 'epoch,swap_used_mb,dolt_sqlserver_rss_mb,dolt_sqlserver_procs\n' >>"$LOGS/mem.csv"
	while [ ! -e "$LOGS/.stop-sampler" ]; do
		now=$(date +%s)
		swap=$(sysctl -n vm.swapusage | sed -E 's/.*used = ([0-9.]+)M.*/\1/')
		rss=$(ps -axo rss=,args= | awk '/[d]olt sql-server/ {s+=$1} END {printf "%d", s/1024}')
		n=$(ps -axo args= | grep -c '[d]olt sql-server')
		printf '%s,%s,%s,%s\n' "$now" "$swap" "${rss:-0}" "$n" >>"$LOGS/mem.csv"
		sleep 5
	done
}
rm -f "$LOGS/.stop-sampler"
sample &
SAMPLER=$!

run_phase() {
	phase=$1
	shift
	echo "=== $phase started $(date -u +%Y-%m-%dT%H:%M:%SZ) ===" | tee -a "$LOGS/run.log"
	start=$(date +%s)
	( "$@" 2>&1; echo "EXIT=$?" >"$LOGS/$phase.exit" ) |
		perl -pe 'BEGIN{$|=1;$s=time} s/^/sprintf("[%7.1fs] ", time-$s)/e' >"$LOGS/$phase.log"
	end=$(date +%s)
	echo "=== $phase finished $(date -u +%Y-%m-%dT%H:%M:%SZ) elapsed=$((end - start))s status=$(cat "$LOGS/$phase.exit" 2>/dev/null) ===" |
		tee -a "$LOGS/run.log"
}

run_phase build "$BIN" brain unify build \
	--host 127.0.0.1 --port "$PORT" \
	--data-dir "$SCRATCH" \
	--allow-collisions \
	--timeout 4h

if [ "$(cat "$LOGS/build.exit" 2>/dev/null)" = "EXIT=0" ]; then
	run_phase verify "$BIN" brain unify verify \
		--host 127.0.0.1 --port "$PORT" \
		--data-dir "$SCRATCH" \
		--timeout 3h
fi

touch "$LOGS/.stop-sampler"
wait "$SAMPLER" 2>/dev/null
echo "=== all phases done ===" | tee -a "$LOGS/run.log"
