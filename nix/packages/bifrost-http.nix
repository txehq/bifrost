{
  pkgs,
  inputs,
  src,
  version,
  bifrost-ui,
}:
let
  lib = pkgs.lib;

  # Bifrost requires Go 1.27 (go.mod/go.work). Force Go 1.27 for buildGoModule.
  buildGoModule = pkgs.callPackage "${inputs.nixpkgs}/pkgs/build-support/go/module.nix" {
    go = pkgs.go_1_27 or pkgs.go;
  };

  transportsLocalReplaces = ''
    if [ -f transports/go.mod ]; then
      cat >> transports/go.mod <<'EOF'

    replace github.com/capsohq/bifrost/core => ../core
    replace github.com/capsohq/bifrost/framework => ../framework
    replace github.com/capsohq/bifrost/plugins/governance => ../plugins/governance
    replace github.com/capsohq/bifrost/plugins/compat => ../plugins/compat
    replace github.com/capsohq/bifrost/plugins/logging => ../plugins/logging
    replace github.com/capsohq/bifrost/plugins/maxim => ../plugins/maxim
    replace github.com/capsohq/bifrost/plugins/otel => ../plugins/otel
    replace github.com/capsohq/bifrost/plugins/semanticcache => ../plugins/semanticcache
    replace github.com/capsohq/bifrost/plugins/telemetry => ../plugins/telemetry
    EOF
    fi
  '';
in
buildGoModule {
  pname = "bifrost-http";
  inherit version;
  inherit src;

  modRoot = "transports";
  subPackages = [ "bifrost-http" ];
  vendorHash = "sha256-Ck1cwv/DYI9EXmp7U2ZSNXlU+Qok8BFn5bcN1Pv7Nmc=";

  doCheck = false;

  overrideModAttrs = final: prev: {
    postPatch = (prev.postPatch or "") + transportsLocalReplaces;
  };

  env = {
    CGO_ENABLED = "1";
  };

  nativeBuildInputs = with pkgs; [
    pkg-config
    gcc
  ];
  buildInputs = [ pkgs.sqlite ];

  postPatch = transportsLocalReplaces;

  preBuild = ''
    # Provide UI assets for //go:embed all:ui
    rm -rf bifrost-http/ui
    mkdir -p bifrost-http/ui
    if [ -d "${bifrost-ui}/ui" ]; then
      cp -R --no-preserve=mode,ownership,timestamps "${bifrost-ui}/ui/." bifrost-http/ui/
    else
      printf '%s\n' '<!doctype html><meta charset="utf-8"><title>Bifrost</title>' > bifrost-http/ui/index.html
    fi
  '';

  ldflags = [
    "-s"
    "-w"
    "-X main.Version=${version}"
  ];

  meta = {
    mainProgram = "bifrost-http";
    description = "Bifrost HTTP gateway";
    homepage = "https://github.com/capsohq/bifrost";
    license = lib.licenses.asl20;
  };
}