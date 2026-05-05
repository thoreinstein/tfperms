# Fixture for catalog entry:
#   - resources/google_logging_project_sink

resource "google_logging_project_sink" "audit" {
  name        = "tfperms-test-sink"
  destination = "storage.googleapis.com/tfperms-test-sink-bucket"
  filter      = "logName : \"audit\""
}
