# Fixture for catalog entries:
#   - resources/google_storage_bucket
#   - data_sources/google_storage_bucket
#
# Both entries share this directory because the catalog's resource and
# data_source sections may legitimately register the same Terraform
# type. The resolver sees the union of both blocks; the golden encodes
# that union.

resource "google_storage_bucket" "primary" {
  name                        = "tfperms-test-bucket"
  location                    = "US"
  uniform_bucket_level_access = true
}

data "google_storage_bucket" "lookup" {
  name = "some-existing-bucket"
}
