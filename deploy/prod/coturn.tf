# coturn — the TURN relay for WebRTC (p2p transfers + room0 voice).
#
# Only the pairs where both sides sit behind symmetric NAT relay through here
# (a fifth to a third of them); everyone else connects directly after STUN.
# The server mints the ephemeral use-auth-secret credentials (server/ice.go),
# so coturn and the gliph server share one static secret and nothing else —
# no per-user state on the relay, natural expiry.
#
# hostNetwork, deliberately: TURN is raw public UDP (3478 + the relay range),
# which a ClusterIP service cannot carry. gliph.sh already resolves straight
# to this node (Porkbun, no proxy), so the relay rides the same hostname and
# needs no new DNS. No TLS listener: WebRTC media is encrypted end-to-end
# (DTLS-SRTP) regardless of the relay leg, and the credential is ephemeral.
#
# The whole thing is gated on turn_secret: unset (the default) deploys nothing
# and the server serves a STUN-only or empty ICE list, exactly as before.

locals {
  coturn_labels = { app = "gliph", component = "coturn" }

  coturn_args = concat(
    [
      "--no-cli",
      "--fingerprint",
      "--realm=${var.host}",
      "--use-auth-secret",
      "--static-auth-secret=${var.turn_secret}",
      "--listening-port=3478",
      "--min-port=${var.turn_min_port}",
      "--max-port=${var.turn_max_port}",
      # A relay must never be a door into the node or the cluster: refuse
      # allocations toward loopback, RFC1918, CGNAT and link-local (cloud
      # metadata lives there).
      "--no-multicast-peers",
      "--denied-peer-ip=127.0.0.0-127.255.255.255",
      "--denied-peer-ip=10.0.0.0-10.255.255.255",
      "--denied-peer-ip=172.16.0.0-172.31.255.255",
      "--denied-peer-ip=192.168.0.0-192.168.255.255",
      "--denied-peer-ip=100.64.0.0-100.127.255.255",
      "--denied-peer-ip=169.254.0.0-169.254.255.255",
      # One misbehaving client cannot eat the relay; the caps comfortably fit
      # the ~40-port relay range.
      "--user-quota=12",
      "--total-quota=100",
    ],
    # Pin listeners and relays to the public interface: without this coturn
    # binds every interface it finds and hands out CNI/docker addresses
    # (10.42.x, 172.17.x) as relay candidates, which no client can reach.
    var.turn_public_ip == "" ? [] : [
      "--listening-ip=${var.turn_public_ip}",
      "--relay-ip=${var.turn_public_ip}",
    ],
  )
}

resource "kubernetes_deployment" "coturn" {
  count = var.turn_secret == "" ? 0 : 1

  metadata {
    name      = "coturn"
    namespace = var.namespace
    labels    = local.coturn_labels
  }

  spec {
    replicas = 1

    # hostNetwork pins the ports; two pods would fight over 3478.
    strategy { type = "Recreate" }

    selector { match_labels = local.coturn_labels }

    template {
      metadata { labels = local.coturn_labels }

      spec {
        host_network = true
        dns_policy   = "ClusterFirstWithHostNet"

        container {
          name  = "coturn"
          image = var.coturn_image
          args  = local.coturn_args

          resources {
            requests = { cpu = "20m", memory = "32Mi" }
            limits   = { memory = "128Mi" }
          }

          # TURN speaks its own protocol; a port probe is the honest check.
          liveness_probe {
            tcp_socket { port = 3478 }
            initial_delay_seconds = 10
            period_seconds        = 30
          }
        }
      }
    }
  }

  depends_on = [kubernetes_namespace.env]
}
