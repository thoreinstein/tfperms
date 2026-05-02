resource "google_storage_bucket" "kept" {
  name = "kept"
}

resource "google_storage_bucket" "dropped" {
  count = 0
  name  = "dropped"
}

resource "google_storage_bucket" "kept_too" {
  count = 1
  name  = "kept_too"
}
