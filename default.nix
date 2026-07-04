{
  buildGoModule,
  buildNpmPackage,
  go,
  lib,
  version ? "0.0.0-dev",
  ...
}: let
  frontend = buildNpmPackage {
    pname = "codex2api-admin";
    inherit version;

    src = ./frontend;
    npmDepsHash = "sha256-NWZZeROBgKSJV9ao9qzy9nODL/8zYdbyN9H0ur7bGKE=";

    VITE_APP_VERSION = version;

    installPhase = ''
      runHook preInstall

      mkdir -p "$out"
      cp -r dist/. "$out/"

      runHook postInstall
    '';
  };
in
  buildGoModule {
    pname = "codex2api";
    inherit version;

    src = ./.;
    vendorHash = "sha256-KRYNXBPTrOHCm+XTdvCSOLyaZEh3Vr4O7F55IjyMUN4=";
    subPackages = ["."];

    # 当前 nixpkgs 的 go_1_26 还是 1.26.3；只在 Nix 构建副本里放宽补丁级 toolchain 要求。
    postPatch = lib.optionalString (lib.versionOlder go.version "1.26.4") ''
      substituteInPlace go.mod \
        --replace-fail 'go 1.26.4' 'go 1.26.3'
    '';

    ldflags = [
      "-s"
      "-w"
    ];

    preBuild = ''
      rm -rf frontend/dist
      mkdir -p frontend/dist
      cp -r ${frontend}/. frontend/dist/
    '';

    doCheck = false;

    meta = {
      description = "Codex2API service with embedded admin frontend";
      homepage = "https://github.com/xingmin1/codex2api";
      license = lib.licenses.mit;
      mainProgram = "codex2api";
      platforms = lib.platforms.unix;
    };
  }
