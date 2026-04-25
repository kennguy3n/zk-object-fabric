variable "env" {
  description = "Deployment environment tag (e.g. prod, staging, beta)."
  type        = string
}

variable "aws_region" {
  description = "AWS region for the control plane resources."
  type        = string
  default     = "us-east-1"
}

variable "vpc_id" {
  description = "Pre-existing VPC ID for RDS, security groups, etc."
  type        = string
}

variable "private_subnet_ids" {
  description = "Three or more private subnet IDs across AZs for RDS."
  type        = list(string)
}

variable "db_password_secret" {
  description = "Secrets Manager ARN holding the RDS master password."
  type        = string
}

variable "rds_instance_class" {
  description = "RDS instance class for the metadata DB."
  type        = string
  default     = "db.r6g.large"
}

variable "clickhouse_url" {
  description = "ClickHouse HTTP(S) URL the gateway sends usage events to."
  type        = string
}

variable "log_retention_days" {
  description = "CloudWatch log group retention for gateway / console."
  type        = number
  default     = 30
}
