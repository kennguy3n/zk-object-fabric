terraform {
  required_version = ">= 1.6.0"

  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 2.18"
    }
  }
}

provider "linode" {
  token = var.linode_token
}

variable "linode_token" {
  type      = string
  sensitive = true
}

variable "region" {
  description = "Linode region (e.g. us-east, us-west, eu-central)."
  type        = string
}

variable "env" {
  description = "Deployment env (prod, staging, beta)."
  type        = string
}

variable "fleet_size" {
  type    = number
  default = 3
}

variable "instance_type" {
  type    = string
  default = "g6-dedicated-8"
}

variable "ssh_authorized_keys" {
  type = list(string)
}

variable "gateway_release_url" {
  description = "HTTPS URL of the signed zk-gateway tarball to install."
  type        = string
}

resource "linode_instance" "gateway" {
  count           = var.fleet_size
  label           = "zkof-${var.env}-${var.region}-gw-${format("%02d", count.index + 1)}"
  region          = var.region
  type            = var.instance_type
  image           = "linode/ubuntu24.04"
  authorized_keys = var.ssh_authorized_keys
  private_ip      = true

  metadata {
    user_data = base64encode(templatefile("${path.module}/cloud-init.yaml.tpl", {
      gateway_release_url = var.gateway_release_url
      env                 = var.env
      region              = var.region
    }))
  }

  tags = ["zkof", "env:${var.env}", "region:${var.region}", "role:gateway"]
}

resource "linode_volume" "cache" {
  count   = var.fleet_size
  label   = "zkof-${var.env}-${var.region}-cache-${format("%02d", count.index + 1)}"
  region  = var.region
  size    = 500
  linode_id = linode_instance.gateway[count.index].id
}

resource "linode_nodebalancer" "fleet" {
  label                = "zkof-${var.env}-${var.region}-nb"
  region               = var.region
  client_conn_throttle = 0
  tags                 = ["zkof", "env:${var.env}", "region:${var.region}"]
}

resource "linode_nodebalancer_config" "https" {
  nodebalancer_id = linode_nodebalancer.fleet.id
  port            = 443
  protocol        = "https"
  cipher_suite    = "recommended"
  algorithm       = "leastconn"
  stickiness      = "none"

  check          = "http"
  check_path     = "/internal/ready"
  check_interval = 5
  check_timeout  = 3
  check_attempts = 2
}

resource "linode_nodebalancer_node" "gateway" {
  count           = var.fleet_size
  nodebalancer_id = linode_nodebalancer.fleet.id
  config_id       = linode_nodebalancer_config.https.id
  label           = linode_instance.gateway[count.index].label
  address         = "${linode_instance.gateway[count.index].private_ip_address}:8080"
  weight          = 100
  mode            = "accept"
}

output "nodebalancer_hostname" {
  value = linode_nodebalancer.fleet.hostname
}

output "instance_ips" {
  value = linode_instance.gateway[*].ip_address
}
