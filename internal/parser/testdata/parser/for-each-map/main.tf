locals {
  buckets = {
    alpha = "us-east1"
    beta  = "us-west1"
  }
}

resource "google_storage_bucket" "many" {
  for_each = local.buckets
  name     = each.key
  location = each.value
}
