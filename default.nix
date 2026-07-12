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
    npmDepsHash = "sha256-yF4LT2ZAZz/wt9OqEMchJxHkg4yNYt9QIB2rdFP3mbM=";

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
    vendorHash = "sha256-y1pBdd3V8LVXoootXyWCvm78p+H00H+tEAlFqMYDfrI=";
    subPackages = ["."];

    # nixpkgs 的 Go 补丁版本可能暂时落后于上游 go.mod；仅在 Nix 构建副本中
    # 对齐到实际构建器版本，避免发布源代码的 toolchain 声明被本地打包需求反向修改。
    postPatch = lib.optionalString (lib.versionOlder go.version "1.26.5") ''
      substituteInPlace go.mod \
        --replace-fail 'go 1.26.5' 'go ${go.version}'
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
