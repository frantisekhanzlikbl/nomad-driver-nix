job "example" {
  datacenters = ["dc1"]
  type        = "batch"

  group "example" {
    task "example" {
      driver = "nix"

      config {
        resolv_conf = "copy-host"
        flake = "/home/manveru/github/input-output-hk/nomad-driver-nix#nixosConfigurations.example"
        command = ["/init"]
        boot = false
        user_namespacing = true
        network_veth = false
        console = "read-only"
        ephemeral = true
      }
    }
  }
}
