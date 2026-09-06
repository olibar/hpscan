#!/bin/sh
# Install (or update) hpscan on a Synology NAS as a Container Manager / Docker
# project, over SSH. Run from the project folder on your Mac:
#   ./deploy/synology-docker-install.sh [user@]inas.local /volume1/Dropbox/ScanDoc iNAS
# Re-run the same command after code changes to rebuild and restart.
set -eu

HOST="${1:?usage: $0 [user@]host scans_dir [name]}"
OUT="${2:?usage: $0 [user@]host scans_dir [name]}"
NAME="${3:-Synology}"
DEST=/volume1/docker/hpscan
# Synology keeps docker in /usr/local/bin, which is not on the PATH of a
# non-interactive SSH session; use absolute paths throughout.
DOCKER=/usr/local/bin/docker
# One SSH connection reused for every step: the password is asked once.
CTL="/tmp/hpscan-ssh-$$"
SSH="ssh -o ControlMaster=auto -o ControlPath=$CTL -o ControlPersist=120"
trap '$SSH -O exit "$HOST" 2>/dev/null || true' EXIT

echo "-> checking Docker on the NAS"
$SSH "$HOST" "test -x $DOCKER || { echo 'docker not found at $DOCKER: install the Docker / Container Manager package'; exit 1; }"
$SSH "$HOST" "test -d '$OUT'" || { echo "scans folder $OUT does not exist on the NAS"; exit 1; }

echo "-> copying project to $HOST:$DEST"
$SSH "$HOST" "mkdir -p $DEST/config"
# Explicit list: bsdtar --exclude patterns match at any depth, so excluding
# the local "hpscan" binary would also drop the cmd/hpscan directory.
COPYFILE_DISABLE=1 tar --no-xattrs -czf - cmd internal go.mod go.sum Dockerfile docker-compose.yml \
    config.yaml README.md Makefile .dockerignore \
  | $SSH "$HOST" "tar -xzf - -C $DEST"

echo "-> configuring"
$SSH "$HOST" "cd $DEST &&
  sed -i 's|- /volume1/scans:/scans|- $OUT:/scans|' docker-compose.yml &&
  if [ ! -f config/config.yaml ]; then cp config.yaml config/config.yaml; fi &&
  sed -i 's|^printer:.*|printer: \"\"|; s|^name:.*|name: \"$NAME\"|; s|^output_dir:.*|output_dir: \"/scans\"|' config/config.yaml &&
  grep -E '^(printer|name|output_dir|format):' config/config.yaml &&
  grep -- '/scans' docker-compose.yml"

echo "-> building and starting the container (sudo password of your NAS user may be asked)"
# sudo resets PATH and the old docker-compose shells out to plain "docker",
# so PATH is passed through explicitly.
$SSH -t "$HOST" "cd $DEST && export PATH=/usr/local/bin:/usr/bin:/bin && \
  if sudo env PATH=\$PATH docker compose version >/dev/null 2>&1; then
    sudo env PATH=\$PATH docker compose up -d --build;
  elif [ -x /usr/local/bin/docker-compose ]; then
    sudo env PATH=\$PATH docker-compose up -d --build;
  else
    echo 'no docker compose found'; exit 1;
  fi"

echo "-> waiting for the client to register on the printer"
sleep 8
$SSH -t "$HOST" "sudo $DOCKER logs --tail 12 hpscan"
cat <<MSG

Done. Manage it in DSM -> Container Manager -> Container -> hpscan (start/stop/logs),
or over SSH:  sudo $DOCKER logs -f hpscan   |   sudo $DOCKER restart hpscan
Config lives on the NAS at $DEST/config/config.yaml (restart the container after editing).
MSG
