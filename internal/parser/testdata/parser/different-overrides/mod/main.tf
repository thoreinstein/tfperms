variable "val" { type = string }

resource "google_storage_bucket" "b" {
  name = "bucket-${var.val}"
}

module "sub" {
  source = "./sub"
  val = var.val
}
