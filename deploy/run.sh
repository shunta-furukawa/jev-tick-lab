#!/bin/sh
# Fetches the TypeSafe key and hands it to the bot through its environment.
#
# This exists because the README's claim — "from Secret Manager, never on disk"
# — was not true of an EnvironmentFile. A file that systemd reads is a plaintext
# key sitting on the boot disk, surviving snapshots and anyone who can read it.
#
# Here the key exists as a shell variable in this process, which then execs the
# bot in place. It is never written anywhere, and `exec` means there is no
# parent process left holding a copy.
#
# Uses only curl and python3, both already on the Debian image, so start-up
# never waits on apt.
set -eu

: "${JEV_PROJECT:?set in /etc/jev-tick-lab/config}"
: "${JEV_SECRET_ID:?set in /etc/jev-tick-lab/config}"
: "${JEV_PAIR:?}" "${JEV_MODE:?}" "${JEV_MODEL:?}" "${JEV_LOG_DIR:?}" "${JEV_TICK:?}"

METADATA="http://metadata.google.internal/computeMetadata/v1"

token=$(curl -fsS -H 'Metadata-Flavor: Google' \
  "$METADATA/instance/service-accounts/default/token" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')

TYPESAFE_API_KEY=$(curl -fsS -H "Authorization: Bearer $token" \
  "https://secretmanager.googleapis.com/v1/projects/$JEV_PROJECT/secrets/$JEV_SECRET_ID/versions/latest:access" |
  python3 -c 'import base64,json,sys; sys.stdout.write(base64.b64decode(json.load(sys.stdin)["payload"]["data"]).decode())')

export TYPESAFE_API_KEY
unset token

# If this failed, set -e already exited and systemd will retry. Starting the bot
# without a key would produce an hour of failed calls that look like an outage.
: "${TYPESAFE_API_KEY:?Secret Manager returned an empty key}"

exec /opt/jev-tick-lab/bot \
  -pair "$JEV_PAIR" \
  -mode "$JEV_MODE" \
  -model "$JEV_MODEL" \
  -tick "$JEV_TICK" \
  -log-dir "$JEV_LOG_DIR"
