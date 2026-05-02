module "b" {
  source = "./mod-b"
}

resource "google_storage_bucket" "in_a" {
  name = "nested-local-in-a"
}
