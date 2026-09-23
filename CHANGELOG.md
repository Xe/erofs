## Unreleased (v0.8.0)

### Features

* Stream compressed builds. `Build` reads each file from its source one time, compresses it one pcluster at a time, and keeps the compressed blocks in a temp file. The memory for a compressed build no longer depends on the size of the files. The image bytes do not change.
* Add `WithSpoolDir` to set the directory of the temp file for compressed builds. The default is `os.TempDir()`.
* `AddFromFS` and `AddFileFunc` now use bounded memory with compression on. `Build` calls `open` one time for each nonempty file, also with compression on.

### Bug Fixes

* `Build` no longer fails with `reading first block for checksum: EOF` when the image is smaller than one block. `Build` now pads the image to the block count in the superblock, as `mkfs.erofs` does.
* A writer that is not an `io.ReaderAt` now gets a correct superblock checksum. Before, `Open` and `Validate` rejected these images.
* `Build` now creates the root directory and parent directories that were not added with `AddDir`. Before, the root had no inode, and implicit subdirectories and their files were missing from the image.
* `AddDir` no longer drops the entries that were added to that directory before the `AddDir` call.

# [0.7.0](https://github.com/Xe/erofs/compare/v0.6.1...v0.7.0) (2026-09-23)


### Features

* **builder:** stream flat file sources during image builds ([#8](https://github.com/Xe/erofs/issues/8)) ([8488f53](https://github.com/Xe/erofs/commit/8488f533a767b895801618f1911201a4a5a8def8))

## Unreleased (v0.7.0)

### Features

* Add `Builder.AddFileFunc` for deferred file sources and bounded-memory flat builds.
* Make `AddFromFS` open regular files during `Build` instead of keeping their contents in memory. A file that changes size after the walk now makes `Build` fail.

## [0.6.1](https://github.com/Xe/erofs/compare/v0.6.0...v0.6.1) (2026-07-10)


### Bug Fixes

* **builder:** pin timestamps to epoch for reproducible images ([#7](https://github.com/Xe/erofs/issues/7)) ([564a966](https://github.com/Xe/erofs/commit/564a966219fb0eb22d72e4478ef024348ffcf991))

# [0.6.0](https://github.com/Xe/erofs/compare/v0.5.0...v0.6.0) (2026-06-28)


### Features

* pack compressed data into big pclusters ([#6](https://github.com/Xe/erofs/issues/6)) ([c162f34](https://github.com/Xe/erofs/commit/c162f3443b65f026604b376206d8222b8ed468fe)), closes [#5](https://github.com/Xe/erofs/issues/5)

# [0.5.0](https://github.com/Xe/erofs/compare/v0.4.0...v0.5.0) (2026-06-28)


### Features

* add Zstandard compression support to the builder ([#4](https://github.com/Xe/erofs/issues/4)) ([8f84bb0](https://github.com/Xe/erofs/commit/8f84bb07549d56a4047e11313f342400ee3520b7))

# [0.4.0](https://github.com/Xe/erofs/compare/v0.3.0...v0.4.0) (2026-04-04)


### Bug Fixes

* address review findings for multi-blob support ([f077542](https://github.com/Xe/erofs/commit/f077542d795b23e8c98cd1b978e6939818949ed7))


### Features

* add --blob flag to erofs-serve for multi-blob images ([7538bf9](https://github.com/Xe/erofs/commit/7538bf94744faf3c6daac33e3299c689f53f0b6e))
* add chunk-based inode building with multi-blob device table ([3858bd6](https://github.com/Xe/erofs/commit/3858bd6229e0398a41968a235988ea7a61352f64))
* add device table parsing for multi-blob support ([7332191](https://github.com/Xe/erofs/commit/7332191ffb8e7e7f28d6ffd0008b77d8395e0e8a))
* add flat device mode for unified address space multi-blob images ([268992d](https://github.com/Xe/erofs/commit/268992d203dc8f6c6323b0efb4a203dcb1b5da5b))
* add OpenMultiBlob constructor and device table parsing in Open ([7edcc3e](https://github.com/Xe/erofs/commit/7edcc3e6c68ec0288b91f89c7d984518df035938))
* dispatch chunk reads to correct blob device by device ID ([d5876e6](https://github.com/Xe/erofs/commit/d5876e66f755a6faf028bbb877713cf9eda4181d))
* display device table in erofs-inspect ([bbe7134](https://github.com/Xe/erofs/commit/bbe71344f247496c12b4cfbf2354e48a8ddd7661))

# [0.3.0](https://github.com/Xe/erofs/compare/v0.2.0...v0.3.0) (2026-04-04)


### Features

* add erofs-inspect command ([b51db2a](https://github.com/Xe/erofs/commit/b51db2a4f157ba40a81d720dcf1dd1592e0ee6b2))

# [0.2.0](https://github.com/Xe/erofs/compare/v0.1.0...v0.2.0) (2026-04-04)


### Features

* add utility commands ([4dc1d04](https://github.com/Xe/erofs/commit/4dc1d044f527764b7f0043fdf985b8df7db61c36))
