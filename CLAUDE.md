# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Go implementation of EROFS (Enhanced Read-Only File System). Provides an `fs.FS` driver for reading EROFS images and a `Builder` for creating them. The reader supports LZ4, LZMA, DEFLATE, and Zstandard decompression; the builder emits LZ4 or Zstandard. The builder packs compressed data into big pclusters (one pcluster spans several lclusters and occupies `ceil(compressed_len / block_size)` physical blocks), so compressed images are actually smaller; the compression level is tunable via `WithCompressionLevel` (zstd levels, or lz4hc for LZ4). Output is intended to be bytewise compatible with the Linux kernel EROFS driver and mkfs.erofs: LZ4 output is validated locally against `fsck.erofs --extract`, but the zstd path (compr_cfgs + zstd big pclusters) is not — the locally installed erofs-utils is built without zstd support, so validate it out of band against erofs-utils >= 1.8.

## Commands

```sh
go build -v ./...          # build all packages
go test -v ./...           # run all tests
go test -v -run TestName   # run a single test
go tool goimports -w .     # format and organize imports
```

Pre-commit hooks run `goimports` on staged `.go` files and execute the test suite via `npx lint-staged` and `npm test`. Install hooks with `npm ci`.

## Skills

When making git commits, use the `conventional-commits` skill. Commit messages must follow Conventional Commits format (enforced by commitlint). PR titles are also linted for conventional format.

## Architecture

The root package (`github.com/Xe/erofs`) contains both the reader and builder:

- **Reader**: `FS` struct implements `fs.FS`, `fs.StatFS`, `fs.ReadLinkFS`. Constructed via `Open()` from any `io.ReaderAt`. All on-disk access is stream-based through `io.ReaderAt` -- no mmap.
- **Builder**: `Builder` struct creates EROFS images via a fluent API with `BuildOption` functional options. Two-phase: accumulate files, then serialize to `io.WriterAt`.
- **Compression**: Pluggable decompressor system in `compress.go` / `builder_compress.go`. Algorithm IDs 0-3 map to LZ4, LZMA, DEFLATE, Zstandard.
- **Path resolution**: Recursive symlink following with a depth limit of 40. Handles both relative and absolute symlinks.

`internal/ondisk` defines all binary on-disk structures (`SuperBlock`, `InodeCompact`, `InodeExtended`, `Dirent`, etc.) and format constants. These map directly to the Linux kernel's `erofs_fs.h`.

### CLI tools in `cmd/`

- `erofs-inspect` -- dump superblock info and inode statistics
- `erofs-serve` -- serve an EROFS image over HTTP
- `mkfs.erofs` -- create EROFS images from a directory

### Test data

`testdata/toybox.img` is a real EROFS image used by integration tests. Builder tests use round-trip verification (create then read back).

### Reference kernel source

The untracked `kernel/` directory contains the Linux kernel EROFS driver source used as a specification reference. It is not part of the build.
