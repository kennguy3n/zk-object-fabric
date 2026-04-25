# Top-level Terraform composition for the ZK Object Fabric AWS
# control plane. Each sub-module is independently usable so an
# operator can apply RDS first, configure the gateway, and only
# then layer on the CloudWatch surface.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.40"
    }
  }
}

provider "aws" {
  region = var.aws_region
}

module "rds" {
  source = "./rds"

  env                = var.env
  aws_region         = var.aws_region
  vpc_id             = var.vpc_id
  private_subnet_ids = var.private_subnet_ids
  db_password_secret = var.db_password_secret
  instance_class     = var.rds_instance_class
}

module "iam" {
  source = "./iam"

  env             = var.env
  kms_key_arn     = aws_kms_key.cmk.arn
  rds_resource_id = module.rds.resource_id
  clickhouse_url  = var.clickhouse_url
}

resource "aws_kms_key" "cmk" {
  description             = "ZK Object Fabric CMK (${var.env})"
  enable_key_rotation     = true
  deletion_window_in_days = 30

  tags = {
    service = "zk-object-fabric"
    env     = var.env
  }
}

resource "aws_kms_alias" "cmk" {
  name          = "alias/zkof-cmk-${var.env}"
  target_key_id = aws_kms_key.cmk.key_id
}

module "cloudwatch" {
  source = "./cloudwatch"

  env                = var.env
  aws_region         = var.aws_region
  log_group_retention = var.log_retention_days
}

output "rds_endpoint" {
  value     = module.rds.endpoint
  sensitive = false
}

output "kms_key_arn" {
  value = aws_kms_key.cmk.arn
}

output "gateway_role_arn" {
  value = module.iam.gateway_role_arn
}

output "console_role_arn" {
  value = module.iam.console_role_arn
}
