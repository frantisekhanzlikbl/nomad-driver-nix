job "nixos" {
  datacenters = ["dc1"]
  type        = "batch"

  group "nixos" {
    task "nixos" {
      driver = "nix"

      resources {
        memory = 1000
        cpu = 3000
      }

      config {
        nixos = "/home/manveru/github/input-output-hk/nomad-driver-nix#nixosConfigurations.example"
      }
    }
  }
}
