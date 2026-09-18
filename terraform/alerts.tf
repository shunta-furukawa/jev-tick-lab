# Alerting. Optional — set alert_email to switch it on.
#
# Two policies, because there are two ways to lose a run and they look nothing
# alike in the logs:
#
#   1. logcheck ran and said the data is degraded.
#   2. logcheck did not run at all — the VM is gone, wedged, or never booted.
#
# The second is the one that silently costs a week, and it cannot be caught by
# matching on a log line, because the point is that no log line arrives.

locals {
  alerting = var.alert_email != "" ? 1 : 0
}

resource "google_monitoring_notification_channel" "email" {
  count        = local.alerting
  display_name = "${var.name} alerts"
  type         = "email"

  labels = {
    email_address = var.alert_email
  }
}

# Every health verdict, healthy or not, counted. This metric existing is what
# makes "nothing reported" detectable.
#
# The filter is a full-text match rather than a jsonPayload field lookup: the
# ops agent forwards the journal line as text unless it has been configured to
# parse it, and a filter that silently matches nothing is worse than no alert.
# Confirm it against real entries after the first hour:
#
#   gcloud logging read '"\"msg\":\"logcheck\""' --limit 5 --freshness 2h
resource "google_logging_metric" "health_reports" {
  count  = local.alerting
  name   = "${var.name}-health-reports"
  filter = <<-EOT
    resource.type="gce_instance"
    "\"msg\":\"logcheck\""
  EOT

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
  }
}

resource "google_monitoring_alert_policy" "degraded" {
  count        = local.alerting
  display_name = "${var.name}: collection degraded"
  combiner     = "OR"

  documentation {
    content = "logcheck reported the last hour of tick data as unusable. See docs/operations.md for what each problem means."
  }

  conditions {
    display_name = "logcheck reported degraded data"

    condition_matched_log {
      filter = <<-EOT
        resource.type="gce_instance"
        "\"msg\":\"logcheck\""
        "\"healthy\":false"
      EOT
    }
  }

  alert_strategy {
    # A log-match policy needs this. One mail an hour is plenty: logcheck only
    # runs hourly, and a degraded run stays degraded until somebody acts.
    notification_rate_limit {
      period = "3600s"
    }
  }

  notification_channels = [google_monitoring_notification_channel.email[0].id]
}

resource "google_monitoring_alert_policy" "silent" {
  count        = local.alerting
  display_name = "${var.name}: no health report"
  combiner     = "OR"

  documentation {
    content = "No health verdict has arrived for three hours. The VM, the collector, or the logcheck timer has stopped. This is the failure that quietly costs a week of collection."
  }

  conditions {
    display_name = "no logcheck output for 3h"

    condition_absent {
      filter   = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.health_reports[0].name}\" AND resource.type=\"gce_instance\""
      duration = "10800s"

      aggregations {
        alignment_period   = "3600s"
        per_series_aligner = "ALIGN_COUNT"
      }
    }
  }

  notification_channels = [google_monitoring_notification_channel.email[0].id]
}
