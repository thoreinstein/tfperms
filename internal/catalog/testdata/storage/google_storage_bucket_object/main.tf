# Fixture for catalog entry:
#   - resources/google_storage_bucket_object

resource "google_storage_bucket_object" "config" {
  name    = "config.json"
  bucket  = "tfperms-test-bucket"
  content = "{}"
}
