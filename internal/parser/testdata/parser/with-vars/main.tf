variable "region" {
  default = "us-east1"
}

variable "no_default" {
  type = string
}

resource "google_storage_bucket" "b" {
  region   = var.region
  fallback = var.no_default
}
