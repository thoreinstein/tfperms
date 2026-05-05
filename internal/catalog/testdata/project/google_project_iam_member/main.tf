# Fixture for catalog entry:
#   - iam_bindings/google_project_iam_member

resource "google_project_iam_member" "alice_viewer" {
  project = "tfperms-test-project"
  role    = "roles/viewer"
  member  = "user:alice@example.com"
}
