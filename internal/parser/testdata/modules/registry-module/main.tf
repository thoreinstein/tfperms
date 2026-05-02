module "consul" {
  source  = "hashicorp/consul/aws"
  version = "0.1.0"
}

resource "google_storage_bucket" "root" {
  name = "registry-root"
}
