# Fixture for catalog entry:
#   - resources/google_cloudfunctions_function

resource "google_cloudfunctions_function" "hello" {
  name                  = "tfperms-test-function"
  runtime               = "nodejs18"
  entry_point           = "helloHttp"
  source_archive_bucket = "tfperms-test-source"
  source_archive_object = "function.zip"
  trigger_http          = true
  available_memory_mb   = 128
}
