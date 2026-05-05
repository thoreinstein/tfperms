# Fixture for catalog entry:
#   - resources/google_pubsub_subscription

resource "google_pubsub_subscription" "events" {
  name  = "tfperms-test-events-sub"
  topic = "tfperms-test-events"
}
