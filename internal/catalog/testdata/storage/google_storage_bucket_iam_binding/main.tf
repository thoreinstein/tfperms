# Fixture for catalog entry:
#   - iam_bindings/google_storage_bucket_iam_binding

resource "google_storage_bucket_iam_binding" "viewers" {
  bucket = "tfperms-test-bucket"
  role   = "roles/storage.objectViewer"
  members = [
    "user:alice@example.com",
  ]
}
