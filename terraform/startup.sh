#!/bin/bash
# Prepares a fresh VM. Runs on every boot, so everything here is idempotent.
#
# This script does NOT install the bot. deploy/deploy.sh does that, so that
# shipping new code never means touching infrastructure. What this does is make
# the machine ready to receive it.
set -euo pipefail

NAME="${name}"
LOG_DIR="/opt/$NAME/data"

# --- user and directories ---------------------------------------------------
# A service account with no login and no shell: this process needs to write one
# directory and talk to two hosts.
if ! id -u jevbot >/dev/null 2>&1; then
  useradd --system --home-dir "/opt/$NAME" --shell /usr/sbin/nologin jevbot
fi

install -d -o root  -g root   -m 0755 "/opt/$NAME"
install -d -o jevbot -g jevbot -m 0750 "$LOG_DIR"
install -d -o root  -g root   -m 0755 "/etc/$NAME"

# --- non-secret configuration ----------------------------------------------
# The API key is deliberately absent. It is fetched at start-up straight into
# the process environment by deploy/run.sh, so it never lands on this disk.
cat >"/etc/$NAME/config" <<CONFIG
# Written by the Terraform startup script. Edit through Terraform, not here.
JEV_PAIR=${pair}
JEV_MODEL=${model}
JEV_MODE=${mode}
JEV_TICK=${tick}
JEV_LOG_DIR=$LOG_DIR
JEV_PROJECT=${project_id}
JEV_SECRET_ID=${secret_id}
JEV_GCS_BUCKET=${log_bucket}
CONFIG
chmod 0644 "/etc/$NAME/config"

# --- packages ---------------------------------------------------------------
# curl and python3 are already on the Debian 12 image and are all that run.sh
# needs, so fetching the key never waits on apt. gcloud is only needed by the
# daily log shipper, which runs hours later.
export DEBIAN_FRONTEND=noninteractive
if ! command -v gcloud >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y -qq google-cloud-cli || echo "startup: google-cloud-cli not installed; the log shipper will fail until it is" >&2
fi

# journald to Cloud Logging, so "did it die at 3am" is answerable without SSH.
if [ ! -f /etc/google-cloud-ops-agent/config.yaml ]; then
  curl -fsSL https://dl.google.com/cloudagents/add-google-cloud-ops-agent-repo.sh -o /tmp/add-ops-agent.sh
  bash /tmp/add-ops-agent.sh --also-install || echo "startup: ops agent install failed; journald stays local" >&2
fi

echo "startup: $NAME ready for deploy"
