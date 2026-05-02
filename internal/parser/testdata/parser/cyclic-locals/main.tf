locals {
  a = local.b
  b = local.a
}

resource "google_storage_bucket" "b" {
  name = "with-cycle"
  ref  = local.a
}
