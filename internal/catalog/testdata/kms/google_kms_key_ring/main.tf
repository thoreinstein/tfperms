# Fixture for catalog entry:
#   - resources/google_kms_key_ring

resource "google_kms_key_ring" "main" {
  name     = "tfperms-test-keyring"
  location = "us-central1"
}
