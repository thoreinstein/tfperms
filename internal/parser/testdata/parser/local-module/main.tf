module "child" {
  source = "./mod"
  env    = "prod"
}

resource "google_storage_bucket" "root_bucket" {
  name = "at-root"
}
