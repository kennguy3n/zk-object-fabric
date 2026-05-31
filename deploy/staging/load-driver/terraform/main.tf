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

# --- Inputs -----------------------------------------------------------

variable "linode_token" {
  description = "Linode API token used for both this module and the gateway-fleet Terraform. Sensitive."
  type        = string
  sensitive   = true
}

variable "region" {
  description = "Linode region. MUST match the gateway-fleet region or the latency measurements reflect WAN RTT instead of the production path."
  type        = string
}

variable "env" {
  description = "Deployment env label; baked into the instance label and tags."
  type        = string
  default     = "staging"
}

variable "instance_type" {
  description = "Linode plan for the load driver. g6-standard-4 is the canonical staging size (4 vCPU, 8 GB RAM); upsize for higher target RPS."
  type        = string
  default     = "g6-standard-4"
}

variable "ssh_authorized_keys" {
  description = "Public SSH keys authorized on the load driver. Operators SSH in to run the harness manually."
  type        = list(string)
}

variable "benchmark_release_url" {
  description = "HTTPS URL of the signed benchmark-runner + tier3-verify tarball. Cloud-init fetches this at first boot."
  type        = string
}

# --- Resources --------------------------------------------------------

resource "linode_instance" "load_driver" {
  label           = "zkof-${var.env}-${var.region}-loaddrv"
  region          = var.region
  type            = var.instance_type
  image           = "linode/ubuntu24.04"
  authorized_keys = var.ssh_authorized_keys
  private_ip      = true

  metadata {
    user_data = base64encode(templatefile("${path.module}/cloud-init.yaml.tpl", {
      benchmark_release_url = var.benchmark_release_url
      env                   = var.env
      region                = var.region
    }))
  }

  tags = [
    "zkof",
    "env:${var.env}",
    "region:${var.region}",
    "role:load-driver",
  ]
}

# --- Outputs ----------------------------------------------------------

output "load_driver_ip" {
  description = "Public IPv4 of the load driver. Use this to SSH in and invoke run_tier3.sh."
  value       = linode_instance.load_driver.ip_address
}

output "load_driver_private_ip" {
  description = "Private IPv4 of the load driver. Use this for the gateway's egress allowlist if a private endpoint is configured."
  value       = linode_instance.load_driver.private_ip_address
}

output "load_driver_label" {
  description = "Linode instance label of the load driver. Used by collect_evidence.sh to fetch the journalctl logs."
  value       = linode_instance.load_driver.label
}
