variable "hcloud_token" {
  sensitive = true
  type = string
}

variable "garm_db_password" {
  sensitive = true
  type = string
}

variable "github_pat" {
  sensitive = true
  type = string
}

variable "prefix" {
  type = string
}

variable "base_domain" {
  type = string
}

variable "github_user" {
  type = string
}