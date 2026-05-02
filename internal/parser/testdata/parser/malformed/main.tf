resource "google_storage_bucket" "broken" {
  name = "missing-closing-brace"
