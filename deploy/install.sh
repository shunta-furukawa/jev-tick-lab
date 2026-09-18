#!/bin/bash
# Installs an uploaded payload. Runs ON the VM, via deploy.sh; not meant to be
# run by hand.
set -euo pipefail

NAME=jev-tick-lab
SRC="${1:-/tmp/$NAME-deploy}"

if [ ! -d "$SRC" ]; then
  echo "install: $SRC does not exist; run deploy/deploy.sh instead" >&2
  exit 1
fi
if [ ! -f "/etc/$NAME/config" ]; then
  echo "install: /etc/$NAME/config is missing; has the Terraform startup script run?" >&2
  exit 1
fi

install -m 0755 -o root -g root "$SRC/bot"      "/opt/$NAME/bot"
install -m 0755 -o root -g root "$SRC/logcheck" "/opt/$NAME/logcheck"
install -m 0755 -o root -g root "$SRC/run.sh"       "/opt/$NAME/run.sh"
install -m 0755 -o root -g root "$SRC/ship-logs.sh" "/opt/$NAME/ship-logs.sh"

install -m 0644 -o root -g root "$SRC"/*.service "$SRC"/*.timer /etc/systemd/system/

systemctl daemon-reload
systemctl enable "$NAME-logcheck.timer" "$NAME-ship.timer"
systemctl restart "$NAME-logcheck.timer" "$NAME-ship.timer"

# Restarting the collector loses a few seconds of ticks, which is why this is
# the last thing that happens and why deploys should be rare during a run.
systemctl enable "$NAME.service"
systemctl restart "$NAME.service"

sleep 3
systemctl is-active --quiet "$NAME.service" && echo "install: $NAME is running" || {
  echo "install: $NAME failed to start:" >&2
  journalctl -u "$NAME.service" -n 30 --no-pager >&2
  exit 1
}
