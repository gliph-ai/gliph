# Gliph production — https://gliph.sh
#
# One container: the Go server owns /api, /ws and serves the built web client
# off the same origin. State is a SQLite file plus the upload spool, both on a
# node-local hostPath volume, so this is a single replica with a Recreate
# rollout — never two pods on one database file.
#
# Shape follows inbox2/deploy/prod: kubernetes provider against the shared RKE2
# node, one namespace per environment, a registry pull secret, nginx ingress.
# It differs in TLS: gliph.sh resolves straight to the node (Porkbun DNS, no
# Cloudflare proxy in front), so cert-manager issues the certificate here.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.31"
    }
  }
}

provider "kubernetes" {
  config_path    = var.kubeconfig_path
  config_context = var.kubeconfig_context
}

locals {
  labels = { app = "gliph", component = "server" }
  hosts  = concat([var.host], var.extra_hosts)

  # Flags, not env vars: the server is configured entirely on its command line
  # (see server/main.go). Empty values are dropped so an unset relay stays
  # genuinely unset rather than becoming an empty flag.
  flags = concat(
    # -downloads: the desktop installers bin/release-desktop copies into the pod,
    # served at /downloads/desktop for first installs and for the in-app updater.
    ["-addr", ":8787", "-data", "/data", "-web", "/srv/web", "-name", var.host, "-downloads", "/data/downloads"],
    var.push_relay == "" ? [] : ["-push-relay", var.push_relay],
    var.apns_key == "" ? [] : ["-apns-key", "/secrets/apns.p8", "-apns-key-id", var.apns_key_id, "-apns-team-id", var.apns_team_id],
    # The SES secret rides on the command line the way -turn-secret already
    # does. On a single-operator cluster that is not a weaker place to put it:
    # anyone who can read this Deployment can read the namespace's Secrets too.
    # If gliph ever shares a cluster with people who should not have the shared
    # AWS credential, move it to the mounted-file pattern the APNs key uses.
    var.ses_access_key_id == "" ? [] : ["-ses-key-id", var.ses_access_key_id, "-ses-secret", var.ses_secret_access_key, "-ses-region", var.ses_region, "-mail-from", var.mail_from],
    var.stun_url == "" ? [] : ["-stun", var.stun_url],
    var.turn_url == "" ? [] : ["-turn", var.turn_url, "-turn-secret", var.turn_secret],
  )
}

resource "kubernetes_namespace" "env" {
  metadata { name = var.namespace }
}

resource "kubernetes_secret" "registry" {
  metadata {
    name      = "registry"
    namespace = var.namespace
  }

  type = "kubernetes.io/dockerconfigjson"

  data = {
    ".dockerconfigjson" = jsonencode({
      auths = {
        (var.registry_server) = {
          auth = base64encode("${var.registry_username}:${var.registry_password}")
        }
      }
    })
  }

  depends_on = [kubernetes_namespace.env]
}

# The APNs auth key, when there is one. Empty data makes an empty file the
# server is never pointed at (the flag is dropped above).
resource "kubernetes_secret" "apns" {
  metadata {
    name      = "apns"
    namespace = var.namespace
  }
  data       = { "apns.p8" = var.apns_key }
  depends_on = [kubernetes_namespace.env]
}

# ── State ───────────────────────────────────────────────────────────────────
# gliph.db (SQLite, WAL) + data/uploads. Retain: deleting the release must not
# take the message history with it.
resource "kubernetes_persistent_volume" "data" {
  metadata { name = "${var.namespace}-data" }

  spec {
    capacity                         = { storage = var.storage_size }
    access_modes                     = ["ReadWriteOnce"]
    persistent_volume_reclaim_policy = "Retain"

    persistent_volume_source {
      host_path {
        path = var.data_host_path
        type = "DirectoryOrCreate"
      }
    }
  }
}

resource "kubernetes_persistent_volume_claim" "data" {
  metadata {
    name      = "data"
    namespace = var.namespace
  }

  spec {
    access_modes = ["ReadWriteOnce"]
    volume_name  = kubernetes_persistent_volume.data.metadata[0].name
    resources { requests = { storage = var.storage_size } }
  }

  depends_on = [kubernetes_namespace.env]
}

resource "kubernetes_deployment" "server" {
  metadata {
    name      = "gliph"
    namespace = var.namespace
    labels    = local.labels
  }

  # CI owns the running image, not Terraform. Pushing to main builds
  # gliph:<sha> and repins the deployment (docs/deploy.md), so with both of
  # them writing this one field every apply had to be preceded by copying the
  # live sha into tfvars by hand, and an apply that forgot rolled production
  # back to whatever the file still said. var.image is what a NEW deployment
  # starts on; after that the field is CI's. Roll back by re-running a build,
  # the way every other release moves.
  lifecycle {
    ignore_changes = [spec[0].template[0].spec[0].container[0].image]
  }

  spec {
    replicas = 1

    # One SQLite writer. Never let a new pod start before the old one is gone.
    strategy { type = "Recreate" }

    selector { match_labels = local.labels }

    template {
      metadata { labels = local.labels }

      spec {
        image_pull_secrets { name = kubernetes_secret.registry.metadata[0].name }

        container {
          name              = "gliph"
          image             = var.image
          image_pull_policy = "Always"
          args              = local.flags

          port {
            container_port = 8787
            name           = "http"
          }

          volume_mount {
            name       = "data"
            mount_path = "/data"
          }

          volume_mount {
            name       = "apns"
            mount_path = "/secrets"
            read_only  = true
          }

          resources {
            requests = { cpu = "50m", memory = "128Mi" }
            limits   = { memory = "1Gi" }
          }

          readiness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            initial_delay_seconds = 5
            period_seconds        = 10
          }

          liveness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            initial_delay_seconds = 30
            period_seconds        = 30
          }
        }

        volume {
          name = "data"
          persistent_volume_claim {
            claim_name = kubernetes_persistent_volume_claim.data.metadata[0].name
          }
        }

        volume {
          name = "apns"
          secret { secret_name = kubernetes_secret.apns.metadata[0].name }
        }
      }
    }
  }
}

resource "kubernetes_service" "server" {
  metadata {
    name      = "gliph"
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    selector = local.labels
    port {
      port        = 80
      target_port = "http"
    }
  }
}

resource "kubernetes_ingress_v1" "gliph" {
  metadata {
    name      = "gliph"
    namespace = var.namespace

    annotations = {
      "cert-manager.io/cluster-issuer" = var.cluster_issuer
      # Uploads are capped at 1 GiB server-side (Server.UploadMax); nginx must
      # not reject them first.
      "nginx.ingress.kubernetes.io/proxy-body-size" = "1024m"
      # /ws is a long-lived WebSocket. The default 60s read timeout kills it.
      "nginx.ingress.kubernetes.io/proxy-read-timeout" = "3600"
      "nginx.ingress.kubernetes.io/proxy-send-timeout" = "3600"
    }
  }

  spec {
    ingress_class_name = var.ingress_class_name

    tls {
      hosts       = local.hosts
      secret_name = "gliph-tls"
    }

    dynamic "rule" {
      for_each = local.hosts

      content {
        host = rule.value

        http {
          path {
            path      = "/"
            path_type = "Prefix"

            backend {
              service {
                name = kubernetes_service.server.metadata[0].name
                port { number = 80 }
              }
            }
          }
        }
      }
    }
  }
}
