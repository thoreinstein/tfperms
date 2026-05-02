resource "google_storage_bucket" "beta" {
  name = "beta"
}

data "google_project" "current" {}
