resource "google_storage_bucket" "locked" {
  name = "locked"

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_storage_bucket" "unlocked_explicit" {
  name = "unlocked_explicit"

  lifecycle {
    prevent_destroy = false
  }
}

resource "google_storage_bucket" "unlocked_implicit" {
  name = "unlocked_implicit"
}
