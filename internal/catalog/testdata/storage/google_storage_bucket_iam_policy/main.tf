# Fixture for catalog entry:
#   - iam_bindings/google_storage_bucket_iam_policy
#
# Inlining the policy JSON via jsonencode rather than chaining through
# `data "google_iam_policy"` keeps the fixture surface to a single
# catalog entry — pulling in a separate (unmodelled) data source would
# surface as an "unknown" in the golden and obscure the binding test.

resource "google_storage_bucket_iam_policy" "policy" {
  bucket = "tfperms-test-bucket"
  policy_data = jsonencode({
    bindings = [
      {
        role    = "roles/storage.objectViewer"
        members = ["user:alice@example.com"]
      },
    ]
  })
}
