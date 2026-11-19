variable "kubeconfig_path" {
  type    = string
  default = "./kubeconfig.yaml"
}

variable "kubeconfig_context" {
  type    = string
  default = null
}

variable "namespace" {
  type    = string
  default = "gliph-prod"
}

variable "host" {
  type        = string
  default     = "gliph.sh"
  description = "Public host. Also passed as -name, so it is the server name clients see."
}

variable "extra_hosts" {
  type        = list(string)
  default     = ["www.gliph.sh"]
  description = "Additional hosts served by the same ingress and covered by the same certificate."
}

variable "image" {
  type    = string
  default = "registry.erip.me/gliph/gliph:latest"
}

variable "registry_server" {
  type    = string
  default = "registry.erip.me"
}

variable "registry_username" { type = string }

variable "registry_password" {
  type      = string
  sensitive = true
}

variable "data_host_path" {
  type        = string
  default     = "/var/lib/gliph-prod"
  description = "Node-local directory holding gliph.db and the upload spool."
}

variable "storage_size" {
  type    = string
  default = "20Gi"
}

variable "ingress_class_name" {
  type    = string
  default = "nginx"
}

variable "cluster_issuer" {
  type        = string
  default     = "letsencrypt-prod"
  description = "cert-manager ClusterIssuer. gliph.sh points straight at the node, so TLS terminates here."
}

# ── Optional services ───────────────────────────────────────────────────────
# All unset by default: mobile push is simply unavailable without a relay, and
# peer-to-peer transfers fall back to the server without STUN/TURN.

variable "push_relay" {
  type    = string
  default = ""
}

# Direct APNs for iOS: the .p8 auth key's contents, its id, and the team. Set
# all three or none; the key is mounted as a file the server reads at start.
variable "apns_key" {
  type      = string
  default   = ""
  sensitive = true
}

variable "apns_key_id" {
  type    = string
  default = ""
}

variable "apns_team_id" {
  type    = string
  default = "KF53B4S5HD"
}

# Outbound email, on the shared devops@erip.me SES account (eu-west-1, out of
# the sandbox). Only the missed-message digest uses it, and what it sends is
# counts and channel names — the server holds ciphertext and cannot write a
# preview even if we wanted one. Unset, gliph sends no mail at all.
#
# mail_from must be an identity verified in that account. gliph.sh is NOT
# verified yet, so the working value today is devops@erip.me; verify gliph.sh as
# a domain identity (DKIM CNAMEs) and this becomes noreply@gliph.sh.
variable "ses_access_key_id" {
  type      = string
  default   = ""
  sensitive = true
}

variable "ses_secret_access_key" {
  type      = string
  default   = ""
  sensitive = true
}

variable "ses_region" {
  type    = string
  default = "eu-west-1"
}

variable "mail_from" {
  type    = string
  default = ""
}

variable "stun_url" {
  type    = string
  default = ""
}

variable "turn_url" {
  type    = string
  default = ""
}

variable "turn_secret" {
  type      = string
  default   = ""
  sensitive = true
}

# coturn (deployed only when turn_secret is set — see coturn.tf).
variable "coturn_image" {
  type    = string
  default = "docker.io/coturn/coturn:4.7"
}

variable "turn_min_port" {
  type    = number
  default = 49160
}

variable "turn_max_port" {
  type    = number
  default = 49200
}

variable "turn_public_ip" {
  type        = string
  default     = ""
  description = "The node's public IPv4. Pins coturn's listeners and relays to it, keeping cluster-internal interfaces out of the candidate list."
}
