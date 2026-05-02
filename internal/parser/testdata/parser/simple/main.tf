resource "google_storage_bucket" "b" {
  name     = "my-bucket"
  location = "US"
}

data "google_project" "current" {}
