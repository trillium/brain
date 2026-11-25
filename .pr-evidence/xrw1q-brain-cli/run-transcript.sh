#!/bin/sh
# Drives the store-host default against a throwaway HOME. Nothing here touches
# a real store: the addresses are RFC 5737 documentation addresses, and
# `dolt show` only prints what bd resolved.
set -u
OLD=$1; NEW=$2; H=$3
mkdir -p "$H/data/tasks/.beads" "$H/data/notes/.beads" "$H/.config/brain" "$H/bin"
chmod 700 "$H/data/tasks/.beads" "$H/data/notes/.beads"
printf '%s\n' '{"backend":"dolt","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_database":"brain_unified","project_id":"00000000-0000-4000-8000-000000000001"}' > "$H/data/tasks/.beads/metadata.json"
echo 3399 > "$H/data/tasks/.beads/dolt-server.port"
printf '%s\n' '{"backend":"dolt","dolt_mode":"embedded","project_id":"00000000-0000-4000-8000-000000000002"}' > "$H/data/notes/.beads/metadata.json"
ln -sf "$OLD" "$H/bin/bd-before"; ln -sf "$NEW" "$H/bin/bd"
# The whole store wrapper: no host, no port, nothing sourced.
cat > "$H/bin/task" <<'W'
#!/bin/sh
export BEADS_DIR="$HOME/data/tasks/.beads"
export BD_NAME=task
exec "${BD:-bd}" "$@"
W
cat > "$H/bin/notes" <<'W'
#!/bin/sh
export BEADS_DIR="$HOME/data/notes/.beads"
export BD_NAME=notes
exec bd "$@"
W
chmod +x "$H/bin/task" "$H/bin/notes"
F="$H/.config/brain/store-host.env"
show() { grep -E '^(beads:|Error:|exit |  (Host|Port|User|Mode):)' | sed "s|$H|~|g"; }
sh_() { echo "\$ $1"; env -i HOME="$H" PATH="$H/bin:/usr/bin:/bin" BD_DISABLE_METRICS=1 NO_COLOR=1 sh -c "$1" 2>&1 | show; echo; }

echo "# cat ~/bin/task"; cat "$H/bin/task"; echo
echo "## 1. No ~/.config/brain/store-host.env: the store's own setting, as before"
sh_ "task dolt show"

cat > "$F" <<'C'
export BEADS_DOLT_SERVER_HOST=192.0.2.10
export BEADS_DOLT_SERVER_PORT=3307
export BEADS_DOLT_SERVER_USER=brain_app
C
echo "## 2. The machine's default is written"
echo "\$ cat ~/.config/brain/store-host.env"; cat "$F"; echo
echo "### before this change the binary ignores it (only a wrapper that sourced it saw it)"
sh_ "BD=bd-before task dolt show"
echo "### after: the same wrapper reaches the machine's default"
sh_ "task dolt show"

echo "## 3. The environment overrides the file"
sh_ "BEADS_DOLT_SERVER_HOST=192.0.2.20 BEADS_DOLT_SERVER_PORT=3355 task dolt show"

echo "## 4. --store-host overrides the environment and the file"
sh_ "BEADS_DOLT_SERVER_HOST=192.0.2.20 BEADS_DOLT_SERVER_PORT=3355 task --store-host 192.0.2.30:3311 dolt show"

echo "## 5. A bare bd is not a store command: the file is not read"
sh_ "BEADS_DIR=~/data/tasks/.beads bd dolt show"

echo "## 6. A store with its own embedded Dolt is left alone"
sh_ "notes dolt show"

echo "## 7. A bad line is named, the rest of the file still applies (3307 is bd's own default port for a remote host)"
printf '%s\n' 'export BEADS_DOLT_SERVER_HOST=192.0.2.10' 'export BEADS_DOLT_SERVER_PORT=tbd' > "$F"
echo "\$ cat ~/.config/brain/store-host.env"; cat "$F"; echo
sh_ "task dolt show"

echo "## 8. A flag that is not host[:port] is refused"
sh_ "task --store-host 192.0.2.30:sql dolt show; echo \"exit \$?\""
