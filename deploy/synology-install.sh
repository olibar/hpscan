#!/bin/sh
# Install hpscan on a Synology NAS over SSH. Run from the project folder on
# your Mac after `make dist`:
#   ./deploy/synology-install.sh [user@]inas.local /volume1/Dropbox/ScanDoc "iNAS"
set -eu

HOST="${1:?usage: $0 [user@]host output_dir [name]}"
OUT="${2:?usage: $0 [user@]host output_dir [name]}"
NAME="${3:-Synology}"
APP=/volume1/apps/hpscan
CFG="$APP/config.yaml"

echo "-> detecting NAS architecture"
ARCH=$(ssh "$HOST" uname -m)
case "$ARCH" in
  x86_64)          BIN=dist/hpscan-linux-amd64 ;;
  aarch64|arm64)   BIN=dist/hpscan-linux-arm64 ;;
  *) echo "unsupported NAS architecture: $ARCH"; exit 1 ;;
esac
echo "   $ARCH -> $BIN"
[ -f "$BIN" ] || { echo "missing $BIN, run: make dist"; exit 1; }

echo "-> copying binary to $HOST:$APP"
ssh "$HOST" "mkdir -p $APP && rm -f $APP/hpscan"
scp -q "$BIN" "$HOST:$APP/hpscan"
ssh "$HOST" "chmod +x $APP/hpscan"

echo "-> writing config"
ssh "$HOST" "test -f $CFG || HPSCAN_CONFIG=$CFG $APP/hpscan config init >/dev/null;
  HPSCAN_CONFIG=$CFG $APP/hpscan config set printer '' >/dev/null;
  HPSCAN_CONFIG=$CFG $APP/hpscan config set name '$NAME' >/dev/null;
  HPSCAN_CONFIG=$CFG $APP/hpscan config set output_dir '$OUT' >/dev/null;
  HPSCAN_CONFIG=$CFG $APP/hpscan config show"

echo "-> checking the NAS can see the printer"
ssh "$HOST" "HPSCAN_CONFIG=$CFG $APP/hpscan discover" || true

echo "-> installing as a service (sudo password of your NAS user may be asked)"
if ssh -t "$HOST" "sudo HPSCAN_CONFIG=$CFG $APP/hpscan install"; then
  ssh "$HOST" "sudo HPSCAN_CONFIG=$CFG $APP/hpscan status; sleep 6; sudo journalctl -u hpscan -n 8 --no-pager"
else
  cat <<MSG

No systemd on this DSM. Create a boot task instead:
  DSM -> Control Panel -> Task Scheduler -> Create -> Triggered Task -> User-defined script
  Event: Boot-up   User: root   Script:
    HPSCAN_CONFIG=$CFG nohup $APP/hpscan run >> $APP/hpscan.log 2>&1 &
Then right-click the task -> Run.  Check:  tail -f $APP/hpscan.log
MSG
fi
