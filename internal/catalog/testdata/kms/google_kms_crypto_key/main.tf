# Fixture for catalog entry:
#   - resources/google_kms_crypto_key

resource "google_kms_crypto_key" "data_key" {
  name     = "tfperms-test-key"
  key_ring = "projects/example/locations/us-central1/keyRings/tfperms-test-keyring"
  purpose  = "ENCRYPT_DECRYPT"
}
