terraform {
  required_providers {
    http = {
      source = "hashicorp/http"
      version = "3.4.1"
    }
    namecheap = {
      source = "namecheap/namecheap"
      version = "2.1.1"
    }
    hcloud = {
      source = "hetznercloud/hcloud"
      version = "1.44.1"
    }
  }
}

provider "hcloud" {
  token = var.hcloud_token
}

provider "namecheap" {
  # Configuration options
}

provider "http" {
  # Configuration options
}