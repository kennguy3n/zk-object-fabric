variable "env" { type = string }
variable "aws_region" { type = string }
variable "log_group_retention" { type = number }

resource "aws_cloudwatch_log_group" "gateway" {
  name              = "/zkof/${var.env}/gateway"
  retention_in_days = var.log_group_retention

  tags = {
    service = "zk-object-fabric"
    env     = var.env
  }
}

resource "aws_cloudwatch_log_group" "console" {
  name              = "/zkof/${var.env}/console"
  retention_in_days = var.log_group_retention

  tags = {
    service = "zk-object-fabric"
    env     = var.env
  }
}

resource "aws_cloudwatch_metric_alarm" "gateway_5xx" {
  alarm_name          = "zkof-gateway-5xx-rate-${var.env}"
  alarm_description   = "ZK Object Fabric gateway 5xx rate > 1% over 5 minutes"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 5
  metric_name         = "5xx_rate"
  namespace           = "ZKOF/Gateway"
  period              = 60
  statistic           = "Average"
  threshold           = 1.0
  treat_missing_data  = "notBreaching"
}

resource "aws_cloudwatch_metric_alarm" "cache_miss_high" {
  alarm_name          = "zkof-cache-miss-rate-${var.env}"
  alarm_description   = "Cache miss ratio > 50% over 15 minutes"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 15
  metric_name         = "cache_miss_ratio"
  namespace           = "ZKOF/Gateway"
  period              = 60
  statistic           = "Average"
  threshold           = 0.5
  treat_missing_data  = "notBreaching"
}

resource "aws_cloudwatch_metric_alarm" "billing_flush_failure" {
  alarm_name          = "zkof-billing-flush-failure-${var.env}"
  alarm_description   = "ClickHouse billing sink flush errors > 0 over 5 minutes"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 5
  metric_name         = "billing_flush_errors"
  namespace           = "ZKOF/Billing"
  period              = 60
  statistic           = "Sum"
  threshold           = 0
  treat_missing_data  = "notBreaching"
}

resource "aws_cloudwatch_metric_alarm" "abuse_anomaly" {
  alarm_name          = "zkof-abuse-anomaly-rate-${var.env}"
  alarm_description   = "Abuse anomaly alerts > 5/min for 3 minutes"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  metric_name         = "abuse_anomaly_alerts"
  namespace           = "ZKOF/Auth"
  period              = 60
  statistic           = "Sum"
  threshold           = 5
  treat_missing_data  = "notBreaching"
}

output "gateway_log_group" {
  value = aws_cloudwatch_log_group.gateway.name
}
