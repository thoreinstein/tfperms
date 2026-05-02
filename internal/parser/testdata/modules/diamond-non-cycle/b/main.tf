module "common" {
  source = "../shared"
}

resource "google_pubsub_topic" "in_b" {
  name = "diamond-non-cycle-in-b"
}
