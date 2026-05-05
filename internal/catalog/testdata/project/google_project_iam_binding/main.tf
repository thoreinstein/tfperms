# Fixture for catalog entry:
#   - iam_bindings/google_project_iam_binding

resource "google_project_iam_binding" "viewers" {
  project = "tfperms-test-project"
  role    = "roles/viewer"
  members = [
    "user:alice@example.com",
  ]
}
