variable "val" {
  type = string
}

resource "google_storage_bucket" "uses_val" {
  name = var.val
}
