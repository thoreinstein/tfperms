variable "enabled" {
  default = false
}

resource "google_storage_bucket" "gated" {
  count = var.enabled ? 1 : 0
  name  = "module-args-gated"
}
