{ pkgs, lib }:

pkgs.pkgsCross.wasi32.rustPlatform.buildRustPackage {
  pname = "bale-wasm-contract-fixture";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../internal/wasm/testdata;
    fileset = lib.fileset.unions [
      ../internal/wasm/testdata/rust/Cargo.toml
      ../internal/wasm/testdata/rust/Cargo.lock
      ../internal/wasm/testdata/rust/src
      ../internal/wasm/testdata/request.json
      ../internal/wasm/testdata/expected.json
    ];
  };
  cargoRoot = "rust";
  buildAndTestSubdir = "rust";
  cargoLock.lockFile = ../internal/wasm/testdata/rust/Cargo.lock;

  # The target is WASI preview 1, not a native executable. Bale's Go tests
  # exercise the resulting command through its actual WASI host.
  doCheck = false;
  stripAllList = [ "lib" ];
  # Rust's WASI target emits LLD arguments; the cross Cargo hook defaults to cc.
  RUSTFLAGS = "--remap-path-prefix=/build=/bale-testdata --remap-path-prefix=/nix/store=/rust-toolchain -Clinker=${pkgs.llvmPackages.lld}/bin/lld";

  installPhase = ''
    runHook preInstall
    install -Dm444 target/wasm32-wasip1/release/bale-contract-rust.wasm \
      "$out/lib/bale/rust-producer.wasm"
    runHook postInstall
  '';
}
