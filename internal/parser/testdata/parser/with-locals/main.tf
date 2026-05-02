variable "region" {
  default = "us-east1"
}

locals {
  region_copy = var.region
  prefix      = "tfperms"
  full_name   = "${local.prefix}-${local.region_copy}"
}

resource "google_storage_bucket" "b" {
  name   = local.full_name
  region = local.region_copy
  prefix = local.prefix
}
