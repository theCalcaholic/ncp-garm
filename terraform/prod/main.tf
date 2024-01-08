resource "hcloud_server" "garm_server" {

  name        = "${var.prefix}-garm"
  server_type = "cax21"
  datacenter = "hel1-dc2"
  image = "ubuntu-22.04"

  public_net {
    ipv4_enabled = true
  }

  user_data = templatefile("${path.module}/cloudinit.yaml", {
    url = namecheap_domain_records.garm-domain.domain
    jwt_secret = random_password.garm_jwt_secret
    db_password = var.garm_db_password
    volume_name = "${var.prefix}_garm_db"
    github_user = var.github_user
    github_pat = var.github_pat
  })

  ssh_keys = []
}

resource "hcloud_volume" "garm_db" {
  name = "${var.prefix}_garm_db"
  size = 32
  server_id = hcloud_server.garm_server.id
  automount = true
  format = "ext4"
  delete_protection = true
}

resource "namecheap_domain_records" "garm-domain" {
  domain = "${var.base_domain}"
  email_type = "NONE"

  record {
    hostname = "garm"
    type = "A"
    address = "${hcloud_server.garm_server.ipv4_address}"
  }

}

resource "random_password" "garm_jwt_secret" {
  length = 32
}
