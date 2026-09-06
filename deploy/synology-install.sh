#!/bin/sh
# Install hpscan on a Synology NAS over SSH. Run from the project folder on
# your Mac after `make dist`:
#   ./deploy/synology-install.sh [user@]nas.local /volume1/scans NAS [run_as_user]
# run_as_user: NAS account that owns the scanned files (default root). It needs
# write permission on the output folder. Use this route rather than Docker when
# Cloud Sync must pick up the scans: Cloud Sync does not see writes made from
# inside containers.
set -eu

HOST="${1:?usage: $0 [user@]host output_dir [name] [run_as_user]}"
OUT="${2:?usage: $0 [user@]host output_dir [name] [run_as_user]}"
NAME="${3:-Synology}"
RUNAS="${4:-}"
CTL="/tmp/hpscan-ssh-$$"
SSH="ssh -o ControlMaster=auto -o ControlPath=$CTL -o ControlPersist=120"
trap '$SSH -O exit "$HOST" 2>/dev/null || true' EXIT
APP=/volume1/apps/hpscan
CFG="$APP/config.yaml"

echo "-> detecting NAS architecture"
ARCH=$($SSH "$HOST" uname -m)
case "$ARCH" in
  x86_64)          BIN=dist/hpscan-linux-amd64 ;;
  aarch64|arm64)   BIN=dist/hpscan-linux-arm64 ;;
  *) echo "unsupported NAS architecture: $ARCH"; exit 1 ;;
esac
echo "   $ARCH -> $BIN"
[ -f "$BIN" ] || { echo "missing $BIN, run: make dist"; exit 1; }

echo "-> copying binary to $HOST:$APP (sudo password may be asked)"
$SSH -t "$HOST" "sudo mkdir -p $APP && sudo chown \$(id -un) $APP && sudo rm -f $APP/hpscan"
# scp needs the SFTP service, which DSM disables by default: stream over ssh.
$SSH "$HOST" "cat > $APP/hpscan && chmod +x $APP/hpscan" < "$BIN"
$SSH "$HOST" "ls -la $APP/hpscan"

echo "-> writing config (sudo password may be asked)"
$SSH -t "$HOST" "sudo sh -c 'test -f $CFG || HPSCAN_CONFIG=$CFG $APP/hpscan config init >/dev/null;
  HPSCAN_CONFIG=$CFG $APP/hpscan config set printer \"\" >/dev/null;
  HPSCAN_CONFIG=$CFG $APP/hpscan config set name $NAME >/dev/null;
  HPSCAN_CONFIG=$CFG $APP/hpscan config set output_dir $OUT >/dev/null;
  grep -E \"^(printer|name|output_dir|format):\" $CFG'"

echo "-> removing any Docker instance so only one client registers as $NAME (sudo password may be asked)"
$SSH -t "$HOST" "sudo /usr/local/bin/docker rm -f hpscan >/dev/null 2>&1 || true"
USERFLAG=""
if [ -n "$RUNAS" ]; then
  echo "-> files will be owned by $RUNAS"
  $SSH -t "$HOST" "sudo chown -R $RUNAS $APP"
  USERFLAG="--user $RUNAS"
fi

echo "-> checking the NAS can see the printer"
$SSH "$HOST" "HPSCAN_CONFIG=$CFG $APP/hpscan discover" || true

echo "-> installing as a service (sudo password of your NAS user may be asked)"
if $SSH -t "$HOST" "sudo HPSCAN_CONFIG=$CFG $APP/hpscan install $USERFLAG"; then
  $SSH -t "$HOST" "sudo HPSCAN_CONFIG=$CFG $APP/hpscan status; sleep 6; sudo journalctl -u hpscan -n 8 --no-pager"
else
  cat <<MSG

No systemd on this DSM. Create a boot task instead:
  DSM -> Control Panel -> Task Scheduler -> Create -> Triggered Task -> User-defined script
  Event: Boot-up   User: root   Script:
    nohup su ${RUNAS:-root} -s /bin/sh -c "$APP/hpscan run --config $CFG" >> $APP/hpscan.log 2>&1 &
Then right-click the task -> Run.  Check:  tail -f $APP/hpscan.log
MSG
fi
