# Fixture for catalog entry:
#   - iam_bindings/google_project_iam_policy

data "google_iam_policy" "project_admin" {
  binding {
    role    = "roles/owner"
    members = ["user:owner@example.com"]
  }
}

resource "google_project_iam_policy" "project" {
  project     = "tfperms-test-project"
  policy_data = data.google_iam_policy.project_admin.policy_data
}
