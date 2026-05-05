# Fixture for catalog entry:
#   - resources/google_storage_notification

resource "google_storage_notification" "object_changed" {
  bucket         = "tfperms-test-bucket"
  topic          = "projects/example/topics/tfperms-test-topic"
  payload_format = "JSON_API_V1"
}
