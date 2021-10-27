job "nixpkgs" {
  datacenters = ["dc1"]
  type        = "batch"

  group "nixpkgs" {
    task "nixpkgs" {
      driver = "nix"

      resources {
        memory = 1000
        cpu = 3000
      }

      config {
        packages = [
          "github:nixos/nixpkgs/nixos-21.05#bash",
          "github:nixos/nixpkgs/nixos-21.05#coreutils"
        ]
        command = ["/bin/bash", "-c", "sleep 6000"]
      }
    }
  }
}
