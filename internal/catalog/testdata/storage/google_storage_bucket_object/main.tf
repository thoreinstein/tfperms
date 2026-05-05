# Fixture for catalog entry:
#   - resources/google_storage_bucket_object
#
# This fixture also exercises the resolver's `lifecycle { prevent_destroy
# = true }` branch: with the literal flag set, the resolver MUST NOT
# include `storage.objects.delete` in the apply set, even though the
# catalog entry declares it. The expected.json golden encodes that
# omission. Removing prevent_destroy here without updating the golden
# is a deliberate test break — re-run with `-update` only after
# confirming the resolver behaviour is what you want.

resource "google_storage_bucket_object" "config" {
  name    = "config.json"
  bucket  = "tfperms-test-bucket"
  content = "{}"

  lifecycle {
    prevent_destroy = true
  }
}
