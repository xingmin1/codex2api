{
  description = "Codex2API service and admin frontend";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = {
    self,
    nixpkgs,
  }: let
    lib = nixpkgs.lib;
    systems = [
      "x86_64-linux"
      "aarch64-linux"
      "x86_64-darwin"
      "aarch64-darwin"
    ];
    forAllSystems = lib.genAttrs systems;

    version = lib.strings.trim (builtins.readFile ./VERSION);
  in {
    packages = forAllSystems (
      system: let
        pkgs = import nixpkgs {inherit system;};
        codex2api = pkgs.callPackage ./. {inherit version;};
      in {
        default = codex2api;
        inherit codex2api;
      }
    );

    apps = forAllSystems (system: {
      default = {
        type = "app";
        program = lib.getExe self.packages.${system}.codex2api;
      };
      codex2api = self.apps.${system}.default;
    });

    checks = forAllSystems (system: {
      inherit (self.packages.${system}) codex2api;
    });
  };
}
