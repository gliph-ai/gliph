output "url" {
  value = "https://${var.host}"
}

output "namespace" {
  value = var.namespace
}

output "admin_url" {
  value = "https://${var.host}/admin"
}
