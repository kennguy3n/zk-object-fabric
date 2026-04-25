# RDS PostgreSQL for the gateway's manifest, tenant, auth,
# placement, and dedicated-cell stores. The schema migration lives
# in api/console/schema.sql, internal/auth/schema.sql, and the
# manifest_store/postgres package.

variable "env" { type = string }
variable "aws_region" { type = string }
variable "vpc_id" { type = string }
variable "private_subnet_ids" { type = list(string) }
variable "db_password_secret" { type = string }
variable "instance_class" { type = string }

data "aws_secretsmanager_secret_version" "db" {
  secret_id = var.db_password_secret
}

resource "aws_db_subnet_group" "this" {
  name       = "zkof-${var.env}"
  subnet_ids = var.private_subnet_ids

  tags = {
    service = "zk-object-fabric"
    env     = var.env
  }
}

resource "aws_security_group" "rds" {
  name   = "zkof-rds-${var.env}"
  vpc_id = var.vpc_id

  ingress {
    description = "Postgres from gateway+console SGs"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    self        = true
  }

  egress {
    from_port        = 0
    to_port          = 0
    protocol         = "-1"
    cidr_blocks      = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }

  tags = {
    service = "zk-object-fabric"
    env     = var.env
  }
}

resource "aws_db_instance" "this" {
  identifier              = "zkof-${var.env}"
  engine                  = "postgres"
  engine_version          = "16.4"
  instance_class          = var.instance_class
  allocated_storage       = 100
  max_allocated_storage   = 1000
  storage_type            = "gp3"
  storage_encrypted       = true

  db_name                 = "zkof"
  username                = "zkof_admin"
  password                = data.aws_secretsmanager_secret_version.db.secret_string

  db_subnet_group_name    = aws_db_subnet_group.this.name
  vpc_security_group_ids  = [aws_security_group.rds.id]
  publicly_accessible     = false

  multi_az                = true
  backup_retention_period = 14
  backup_window           = "03:00-04:00"
  maintenance_window      = "Sun:04:00-Sun:05:00"

  performance_insights_enabled = true
  monitoring_interval          = 60
  enabled_cloudwatch_logs_exports = ["postgresql"]

  deletion_protection = true
  skip_final_snapshot = false
  final_snapshot_identifier = "zkof-${var.env}-final"

  tags = {
    service = "zk-object-fabric"
    env     = var.env
  }
}

output "endpoint" {
  value = aws_db_instance.this.endpoint
}

output "resource_id" {
  value = aws_db_instance.this.resource_id
}

output "security_group_id" {
  value = aws_security_group.rds.id
}
