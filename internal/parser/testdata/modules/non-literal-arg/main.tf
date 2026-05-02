variable "no_default_root" {
  type = string
}

module "child" {
  source = "./mod"
  val    = var.no_default_root
}
