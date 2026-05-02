variable "enabled" {
  default = true
}

variable "disabled" {
  default = false
}

resource "google_storage_bucket" "enabled" {
  count = var.enabled ? 1 : 0
  name  = "enabled"
}

resource "google_storage_bucket" "disabled" {
  count = var.disabled ? 1 : 0
  name  = "disabled"
}
