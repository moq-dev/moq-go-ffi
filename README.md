# moq-go-ffi (Go module)

Auto-generated mirror of the raw Go bindings for [Media over QUIC](https://github.com/moq-dev/moq). Most callers want the ergonomic [github.com/moq-dev/moq-go](https://github.com/moq-dev/moq-go) wrapper instead.

Source, issues, and pull requests live in [moq-dev/moq](https://github.com/moq-dev/moq); this repo only carries tagged Go module releases.

## Install

```bash
go get github.com/moq-dev/moq-go-ffi@v0.3.2
```

The module bundles prebuilt native libraries for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` (`libmoq_ffi.a`), and `windows/amd64` (`moq_ffi.lib`); cgo selects the right one automatically.

See [moq-dev/moq/go/ffi/README.md](https://github.com/moq-dev/moq/blob/main/go/ffi/README.md) for usage and the release process.

Licensed under MIT OR Apache-2.0.
