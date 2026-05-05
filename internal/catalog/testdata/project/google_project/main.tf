# Fixture for catalog entry:
#   - resources/google_project

resource "google_project" "default" {
  name       = "tfperms test project"
  project_id = "tfperms-test-project"
}
