The checked-in `rust-producer.wasm` is a WASI preview 1 command compiled from
`rust/`. It tests stdin, Unicode JSON, stdout and errors using a second source
language. Its JSON envelope is test data, not the Bale plugin protocol. Ordinary
Go tests use the checked-in module and do not require Rust.

Rebuild from the repository root with the toolchain and Cargo dependencies
selected by `flake.lock` and `rust/Cargo.lock`:

```sh
nix build --no-update-lock-file path:.#wasm-contract-fixture --out-link result-wasm-contract-fixture
cp result-wasm-contract-fixture/lib/bale/rust-producer.wasm internal/wasm/testdata/rust-producer.wasm
```

The Nix derivation cross-compiles for `wasm32-wasip1`, strips the module and
remaps build, dependency-cache and toolchain paths. It does not install Rust or
use a local rustup toolchain. Bale's WASI contract tests execute the resulting
module, checking both its response and stderr failure behavior.

SHA-256: `b5a2d748f5a3080da6b74a6d2b1481a6494dd959a884fa50fda485b8bbd8834c`.
All fixture source is covered by the repository license.
