#!/bin/bash
# Ships the binaries and the units to an already-provisioned VM.
#
#   ./deploy/deploy.sh PROJECT [INSTANCE] [ZONE]
#
# Terraform builds the machine; this puts code on it. Keeping them apart means a
# code change never risks the bucket holding the collected data, and a VM can be
# rebuilt without a redeploy losing anything.
#
# SSH goes through IAP because the firewall allows port 22 from Google's
# forwarding range only — nothing on this VM answers the open internet.
set -euo pipefail

PROJECT="${1:?usage: deploy.sh PROJECT [INSTANCE] [ZONE]}"
INSTANCE="${2:-jev-tick-lab}"
ZONE="${3:-us-west1-b}"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

cd "$REPO"

# Deploying an untested build into a multi-day collection run is how a week of
# data gets thrown away. SKIP_CHECK=1 for an emergency.
if [ "${SKIP_CHECK:-0}" != "1" ]; then
  echo "==> make check"
  make check
fi

echo "==> building for linux/amd64"
GOOS=linux GOARCH=amd64 go build -o "$STAGE/bot"      ./cmd/bot
GOOS=linux GOARCH=amd64 go build -o "$STAGE/logcheck" ./cmd/logcheck

cp deploy/run.sh deploy/ship-logs.sh deploy/install.sh "$STAGE/"
cp deploy/*.service deploy/*.timer "$STAGE/"

echo "==> uploading to $INSTANCE ($ZONE)"
gcloud compute ssh "$INSTANCE" --project "$PROJECT" --zone "$ZONE" --tunnel-through-iap \
  --command "rm -rf /tmp/jev-tick-lab-deploy && mkdir -p /tmp/jev-tick-lab-deploy"

gcloud compute scp --recurse "$STAGE"/* "$INSTANCE:/tmp/jev-tick-lab-deploy/" \
  --project "$PROJECT" --zone "$ZONE" --tunnel-through-iap

echo "==> installing"
gcloud compute ssh "$INSTANCE" --project "$PROJECT" --zone "$ZONE" --tunnel-through-iap \
  --command "sudo bash /tmp/jev-tick-lab-deploy/install.sh /tmp/jev-tick-lab-deploy"

cat <<NEXT

Deployed. Worth watching for a minute:

  gcloud compute ssh $INSTANCE --project $PROJECT --zone $ZONE --tunnel-through-iap \\
    -- journalctl -u jev-tick-lab -f

And an hour from now, the first health verdict:

  gcloud compute ssh $INSTANCE --project $PROJECT --zone $ZONE --tunnel-through-iap \\
    -- sudo -u jevbot /opt/jev-tick-lab/logcheck -dir /opt/jev-tick-lab/data -window 1h
NEXT
