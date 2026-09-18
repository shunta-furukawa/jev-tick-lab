#!/bin/sh
# Copies tick logs to GCS. Runs hourly, not daily: an hour is how much data a
# dead VM should be able to cost, and re-uploading a growing 90MB file 24 times
# a day costs nothing because ingress to GCS is free.
#
# rsync here never deletes at the destination. The bucket is the archive; the
# disk is a staging area.
set -eu

: "${JEV_GCS_BUCKET:?set in /etc/jev-tick-lab/config}"
: "${JEV_LOG_DIR:?}" "${JEV_PAIR:?}"

exec gcloud storage rsync "$JEV_LOG_DIR" "gs://$JEV_GCS_BUCKET/$JEV_PAIR/" \
  --recursive \
  --no-user-output-enabled
