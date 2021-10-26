job "example" {
  datacenters = ["dc1"]
  type        = "batch"

  group "example" {
    task "example" {
      driver = "nix"

      resources {
        memory = 8000
        cpu = 9000
      }

      config {
        # nixos = "/home/manveru/github/input-output-hk/nomad-driver-nix#nixosConfigurations.example"

        packages = [
          "github:nixos/nixpkgs/nixos-21.05#bash",
          "github:nixos/nixpkgs/nixos-21.05#coreutils"
        ]
        command = ["/bin/bash", "-c", "sleep 6000"]

        resolv_conf = "copy-host"
        boot = false
        user_namespacing = false
        network_veth = false
        console = "read-only"
        ephemeral = true
        process_two = false
        volatile = "overlay"
      }
    }
  }
}
