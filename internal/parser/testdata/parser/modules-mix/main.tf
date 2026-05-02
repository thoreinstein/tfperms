module "local" {
  source = "./child"
  env    = "prod"
}

module "registry" {
  source = "hashicorp/consul/aws"
  version = "0.1.0"
}

module "git" {
  source = "git::https://example.com/repo.git"
}

module "http" {
  source = "https://example.com/module.zip"
}

module "unknown" {
  source = "just-a-string"
}
