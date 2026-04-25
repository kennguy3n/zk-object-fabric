variable "env" { type = string }
variable "kms_key_arn" { type = string }
variable "rds_resource_id" { type = string }
variable "clickhouse_url" { type = string }

# Gateway role: KMS decrypt + ClickHouse write (via VPC endpoint or
# inline egress) + S3 access (Wasabi via static creds, AWS S3 BYOC
# via this role). RDS connect is granted via IAM authentication if
# enabled, or via password from Secrets Manager.
data "aws_iam_policy_document" "gateway_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "gateway" {
  name               = "zkof-gateway-${var.env}"
  assume_role_policy = data.aws_iam_policy_document.gateway_assume.json

  tags = {
    service = "zk-object-fabric"
    env     = var.env
  }
}

data "aws_iam_policy_document" "gateway" {
  statement {
    sid     = "KMSEncryptDecrypt"
    effect  = "Allow"
    actions = ["kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
    resources = [var.kms_key_arn]
  }

  statement {
    sid     = "S3BYOCObjectIO"
    effect  = "Allow"
    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
      "s3:AbortMultipartUpload",
      "s3:ListBucketMultipartUploads",
      "s3:ListBucket",
    ]
    resources = ["*"]
  }

  statement {
    sid     = "RDSConnect"
    effect  = "Allow"
    actions = ["rds-db:connect"]
    resources = [
      "arn:aws:rds-db:*:*:dbuser:${var.rds_resource_id}/zkof_gateway",
    ]
  }

  statement {
    sid     = "CloudWatchPutLogs"
    effect  = "Allow"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "gateway" {
  name   = "zkof-gateway-${var.env}"
  role   = aws_iam_role.gateway.id
  policy = data.aws_iam_policy_document.gateway.json
}

# Console role: read+write on RDS for the auth, tenant, placement,
# and dedicated-cell tables.
resource "aws_iam_role" "console" {
  name               = "zkof-console-${var.env}"
  assume_role_policy = data.aws_iam_policy_document.gateway_assume.json

  tags = {
    service = "zk-object-fabric"
    env     = var.env
  }
}

data "aws_iam_policy_document" "console" {
  statement {
    sid     = "RDSConnect"
    effect  = "Allow"
    actions = ["rds-db:connect"]
    resources = [
      "arn:aws:rds-db:*:*:dbuser:${var.rds_resource_id}/zkof_console",
    ]
  }
  statement {
    sid     = "CloudWatchPutLogs"
    effect  = "Allow"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "console" {
  name   = "zkof-console-${var.env}"
  role   = aws_iam_role.console.id
  policy = data.aws_iam_policy_document.console.json
}

output "gateway_role_arn" {
  value = aws_iam_role.gateway.arn
}

output "console_role_arn" {
  value = aws_iam_role.console.arn
}
