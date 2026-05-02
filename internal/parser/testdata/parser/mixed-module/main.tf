module "near" {
  source = "./local"
}

module "remote" {
  source = "hashicorp/consul/aws"
  version = "0.1.0"
}

resource "google_project" "owner" {
  name = "mixed-root"
}
