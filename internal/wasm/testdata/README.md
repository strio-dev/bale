The prebuilt `rust-producer.wasm` is a standard WASI preview 1 command compiled
from the adjacent `rust/` source. It tests stdin, Unicode JSON, stdout and errors
using a second source language. Its fixture JSON envelope is test data, not the
Bale plugin protocol. Running Go tests does not require Rust.

To reproduce with Rust 1.90.0 and its `wasm32-wasip1` target installed:

```sh
cd internal/wasm/testdata
export RUSTFLAGS="--remap-path-prefix=$PWD=/bale-testdata --remap-path-prefix=${CARGO_HOME:-$HOME/.cargo}=/cargo --remap-path-prefix=$(rustc +1.90.0 --print sysroot)=/rust-toolchain"
cargo +1.90.0 test --locked --manifest-path rust/Cargo.toml
cargo +1.90.0 build --locked --manifest-path rust/Cargo.toml --release --target wasm32-wasip1
cp rust/target/wasm32-wasip1/release/bale-contract-rust.wasm rust-producer.wasm
```

Path remapping keeps local source, dependency-cache and toolchain paths out of
runtime diagnostics in the distributed fixture; stripping debug data alone does
not remove them.

SHA-256: `957d48142a70c8e63e97edb26f8f82487194866813552e8c5b607c60bd192a6e`.
All fixture source is covered by the repository license.
