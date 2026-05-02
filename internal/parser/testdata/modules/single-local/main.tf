module "child" {
  source = "./mod"
}

resource "google_storage_bucket" "root" {
  name = "single-local-root"
}
