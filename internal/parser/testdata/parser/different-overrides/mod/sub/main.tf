variable "val" { type = string }

resource "google_storage_bucket" "sub" {
  name = "sub-bucket-${var.val}"
}
