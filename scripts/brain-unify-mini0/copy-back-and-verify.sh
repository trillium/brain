#!/bin/sh
# Copy the unified database back to the macbook and VERIFY the copy, rather
# than assuming rsync did it.
#
#  1. capture the mini0 original's per-table counts while it is quiescent
#  2. rsync the whole scratch dir back
#  3. per-file SHA-256 manifest comparison (transport integrity)
#  4. open the copy on the macbook and compare every table's row count, plus
#     the build's own accounting tables, against the original
set -eu
DEST=/Users/trilliumsmith/fm_home/firstmate/data/task-4kuun/unified
SRCREM=mini0:/Users/mini0/brain-unify/scratch/unified/
MINI0=/Users/mini0/brain-unify/scratch/unified
M=/opt/homebrew/opt/mysql-client/bin/mysql
DOLT=/opt/homebrew/bin/dolt
MPORT=3392
SPORT=3393

counts() { # counts <port> <db>
	$M -h 127.0.0.1 -P "$1" -u root -N -B -e \
		"select table_name from information_schema.tables where table_schema='$2' and table_type='BASE TABLE' order by table_name" 2>/dev/null |
		while read -r t; do
			n=$($M -h 127.0.0.1 -P "$1" -u root -N -B -e "select count(*) from \`$2\`.\`$t\`" 2>/dev/null || echo ERR)
			printf '%s %s\n' "$t" "$n"
		done
}

echo "=== 1. capture the original on mini0 (quiescent) ==="
ssh -o BatchMode=yes mini0 "nohup $DOLT sql-server -H 127.0.0.1 -P $SPORT --loglevel=error --data-dir $MINI0 >/tmp/uni-srv.log 2>&1 & for i in \$(seq 1 120); do nc -z 127.0.0.1 $SPORT && break; sleep 1; done; nc -z 127.0.0.1 $SPORT && echo up"
counts_via_ssh() {
	ssh -o BatchMode=yes mini0 "M=$M; $M -h 127.0.0.1 -P $SPORT -u root -N -B -e \"select table_name from information_schema.tables where table_schema='brain_unified' and table_type='BASE TABLE' order by table_name\" | while read t; do n=\$($M -h 127.0.0.1 -P $SPORT -u root -N -B -e \"select count(*) from brain_unified.\\\`\$t\\\`\" 2>/dev/null || echo ERR); printf '%s %s\n' \"\$t\" \"\$n\"; done"
}
counts_via_ssh >/tmp/uni_original.txt
echo "tables on original: $(wc -l </tmp/uni_original.txt)"
ssh -o BatchMode=yes mini0 "pkill -f 'data-dir $MINI0' 2>/dev/null; sleep 2; echo stopped"

echo "=== 2. rsync back ==="
mkdir -p "$DEST"
rsync -rlptD "$SRCREM" "$DEST/"
du -sh "$DEST"
find "$DEST" -type f | wc -l

echo "=== 3. per-file SHA-256 manifest ==="
(cd "$DEST" && find . -type f ! -name '*.lock' ! -name 'unified-server.log' ! -name 'dolt-server.lock' | sort | xargs shasum -a 256) >/tmp/manifest_mac.txt
ssh -o BatchMode=yes mini0 "cd $MINI0 && find . -type f ! -name '*.lock' ! -name 'unified-server.log' ! -name 'dolt-server.lock' | sort | xargs shasum -a 256" >/tmp/manifest_mini0.txt
echo "files hashed: macbook=$(wc -l </tmp/manifest_mac.txt) mini0=$(wc -l </tmp/manifest_mini0.txt)"
if diff /tmp/manifest_mac.txt /tmp/manifest_mini0.txt; then
	echo "MANIFEST: IDENTICAL — the copy is byte-for-byte the built database"
else
	echo "MANIFEST: DIFFERS (above)"
fi

echo "=== 4. open the copy and compare row counts ==="
pkill -f "data-dir $DEST/brain_unified" 2>/dev/null || true
"$DOLT" sql-server -H 127.0.0.1 -P "$MPORT" --loglevel=error --data-dir "$DEST/brain_unified" >/tmp/copy-srv.log 2>&1 &
i=0; while [ "$i" -lt 120 ]; do nc -z 127.0.0.1 "$MPORT" 2>/dev/null && break; i=$((i+1)); sleep 1; done
nc -z 127.0.0.1 "$MPORT" || { echo "copy did not open"; exit 1; }
counts "$MPORT" brain_unified >/tmp/uni_copy.txt
echo "tables on copy: $(wc -l </tmp/uni_copy.txt)"
if diff /tmp/uni_original.txt /tmp/uni_copy.txt; then
	echo "ROW COUNTS: IDENTICAL across every table"
else
	echo "ROW COUNTS: DIFFER (above)"
fi

echo "=== 5. the build's own accounting, as read from the copy ==="
for t in brain_unify_import_log brain_unify_source_fingerprints brain_unify_collisions brain_stores brain_store_prefixes; do
	printf '  %-38s %s\n' "$t" "$($M -h 127.0.0.1 -P "$MPORT" -u root -N -B -e "select count(*) from brain_unified.\`$t\`" 2>/dev/null)"
done
echo "  total rows in copy: $(awk '{s+=$2} END {print s}' /tmp/uni_copy.txt)"
