module "near" {
  source = "./local"
}

module "far" {
  source  = "hashicorp/consul/aws"
  version = "0.1.0"
}
