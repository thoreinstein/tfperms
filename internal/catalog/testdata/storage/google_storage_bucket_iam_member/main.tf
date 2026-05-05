# Fixture for catalog entry:
#   - iam_bindings/google_storage_bucket_iam_member

resource "google_storage_bucket_iam_member" "viewer" {
  bucket = "tfperms-test-bucket"
  role   = "roles/storage.objectViewer"
  member = "user:alice@example.com"
}
