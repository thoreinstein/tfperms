# Fixture for catalog entry:
#   - resources/google_compute_instance_group_manager

resource "google_compute_instance_group_manager" "managed" {
  name               = "tfperms-test-mig"
  base_instance_name = "tfperms-mig"
  zone               = "us-central1-a"

  version {
    instance_template = "projects/example/global/instanceTemplates/tfperms-test-template"
  }

  target_size = 1
}
