# Design: a streaming file source for `erofs.Builder`

This spec is for work in the `github.com/Xe/erofs` repository, not in objgit.
Copy it into `docs/superpowers/specs/` of that repository before you start.
Read the `CLAUDE.md` of that repository first.

The code references are to `github.com/Xe/erofs` v0.6.1.

## Problem

`Builder.AddFile(p, info, data []byte)` takes the whole content of a file.
The builder keeps each slice on `buildInode.data` until `Build` returns. The
memory for one build is therefore the total size of every file in the image.

objgit builds one image of a git tree after each push (see
[2026-09-23-erofs-snapshots-design.md](2026-09-23-erofs-snapshots-design.md)).
A 2 GiB tree costs 2 GiB of heap for each build. objgit already reads each
blob through an `io.Reader`, so it does not need the bytes in memory at all.

`AddFromFS` has the same problem. It calls `fs.ReadFile` for each file, so
`mkfs.erofs --dir` holds the whole directory tree in memory.

## Goal

Add a way to give the builder a file as a size and an open function. With
compression off, the memory for a build must not depend on file content. It
must depend only on the count of inodes and directory entries.

## Non-goals

- Streaming for compressed images. With compression on, the builder reads
  lazy files into memory, as it does today. A later spec covers this. See
  "Later: compressed streaming".
- A change to the on-disk format. The output must stay bytewise compatible
  with the kernel driver and with `mkfs.erofs`.
- A change to the reader.

## API

```go
// AddFileFunc adds a regular file whose content comes from open. The builder
// calls open once, during Build, and reads exactly size bytes from it. It
// closes the reader before it calls open for the next file.
func (b *Builder) AddFileFunc(p string, info fs.FileInfo, size int64, open func() (io.ReadCloser, error)) error
```

The rules:

- The builder calls `open` during `Build`, not during `AddFileFunc`.
- The builder calls `open` exactly once for each file. If `size` is 0, it
  does not call `open`.
- If the reader returns fewer or more than `size` bytes, `Build` returns an
  error that names the path, the declared size, and the count of bytes read.
- If `open` or a read returns an error, `Build` returns it, wrapped with the
  path.
- The builder calls the `open` functions in inode order. As a result, only one
  reader is open at a time.

`AddFile` does not change. Internally it can become `AddFileFunc` with an
open function over `bytes.NewReader(data)`. The implementation decides this.

## Why the flat layout streams in one pass

With compression off, the layout of a file depends only on its size:

- `computeLayouts` (`builder.go:550`) picks `InodeFlatInline` or
  `InodeFlatPlain` from `len(ino.data)`, and from nothing else.
- `layoutDataBlocks` (`builder.go:688`) gives each file a start block from
  the same length.

After `layoutDataBlocks` returns, the builder therefore knows two offsets for
each file:

| Part of the file      | Where it goes                                       | Written today by |
| --------------------- | --------------------------------------------------- | ---------------- |
| The full blocks       | `ino.startBlk * blockSize`                          | `writeDataBlocks` (`builder.go:849`) |
| The inline tail       | Directly after the 64-byte inode, at `ino.metaOff + 64` | `writeMetadata` (`builder.go:747`), through `writeInode` (`builder.go:773`) |

One open of the file can fill both parts. Copy the full blocks to the data
offset, then copy the last `size % blockSize` bytes to the tail offset. This
needs one copy buffer and no spool file.

Compare this with the compressed layout. `tryCompressInodes`
(`builder.go:533`) must compress a file to decide if it stays flat. That
decision changes the metadata size, and the metadata size changes every later
NID. The compressed layout cannot stream in one pass. This is the reason that
compressed streaming is a separate spec.

## Changes

1. **Store the source on the inode.** Add `open func() (io.ReadCloser, error)`
   to `buildInode`. `ino.size` already holds the size.

2. **Use `ino.size`, not `len(ino.data)`, for layout.** Replace each
   `len(ino.data)` that means "the file size" in these functions:
   - `computeLayouts`
   - `layoutDataBlocks`
   - `writeInode`
   - `writeDataBlocks`

   Symlinks keep the target in `ino.data`, and `ino.size` already equals its
   length. As a result, this change does not affect them.

3. **Write the inode header without the tail for a lazy file.** In
   `writeInode`, if `ino.open != nil`, write the 64-byte inode and skip the
   inline tail. The tail comes in step 4.

4. **Add a step for lazy file data.** Add `writeLazyData` after
   `writeMetadata`, or merge it into `writeDataBlocks`. For each inode with
   `ino.open != nil` and `ino.size > 0`:
   1. Call `open`.
   2. Copy the full blocks to `ino.startBlk * blockSize` with a
      `blockSize`-sized buffer. For `InodeFlatPlain`, pad the last block with
      zeros, as `writeDataBlocks` does today.
   3. For `InodeFlatInline`, copy the tail bytes to `ino.metaOff + 64`.
   4. Read one more byte. If the read does not return `io.EOF`, return the
      size error.
   5. Close the reader.

   Use a `WriteAt` for each buffer. Do not collect the file in memory.

5. **Keep the compressed path correct.** If `compressEnabled` is true and the
   algorithm is not `CompressionNone`, read each lazy file into `ino.data` at
   the start of `Build`, before `computeLayouts`. Then set `ino.open` to nil.
   The rest of `Build` then runs the existing code. This saves no memory, but
   the output is correct and identical to `AddFile`.

6. **Move `AddFromFS` to the lazy path.** Use `info.Size()` and
   `fsys.Open(p)`. After this change, a file that changes size between the
   walk and `Build` gives the size error. Put this fact in the doc comment of
   `AddFromFS`.

7. **Update the documentation.** The `README.md` gets a short example of
   `AddFileFunc`. The `CHANGELOG.md` gets an entry for the new function and
   for the new behavior of `AddFromFS`.

## The invariant

**An image built with `AddFileFunc` is byte-identical to the same image built
with `AddFile`.** This is true with compression on and with compression off.
Every test below compares the bytes of the two builds.

## Testing

Use table-driven tests, as the repository does today.

`TestAddFileFuncMatchesAddFile` builds each case twice, once with `AddFile`
and once with `AddFileFunc`, and compares the bytes. It also runs `Validate`
on the result and reads each file back through `Open`. Use a block size of
4096, and use these file sizes:

| Case           | Size                     | Layout it covers                     |
| -------------- | ------------------------ | ------------------------------------ |
| empty          | 0                        | Flat inline, no data. `open` is not called. |
| tiny           | 1                        | Flat inline, tail only.              |
| max inline     | 4096 − 64                | The largest tail that fits in the inode block. |
| over inline    | 4096 − 63                | The tail no longer fits. Flat plain. |
| one block      | 4096                     | Flat plain, no tail.                 |
| block plus one | 4097                     | Full block and a tail.               |
| multi block    | 3 × 4096 + 17            | Many full blocks and a tail.         |
| large          | 10 MiB + 5               | The copy loop over many buffers.     |
| mixed tree     | All of the above, in nested directories, with symlinks | The inode order and the offsets between files. |

Run the table three times: with no compression, with
`CompressionAutoLZ4`, and with `CompressionZstd`. The compressed runs prove
step 5.

Other tests:

| Test                           | What it proves                                         |
| ------------------------------ | ------------------------------------------------------ |
| `TestAddFileFuncShortRead`     | A reader that returns `size − 1` bytes gives an error that names the path. |
| `TestAddFileFuncLongRead`      | A reader that returns `size + 1` bytes gives the same error. |
| `TestAddFileFuncOpenError`     | An error from `open` comes out of `Build`, wrapped with the path. |
| `TestAddFileFuncOpensOnce`     | Each `open` runs exactly once. Zero-size files do not call `open`. |
| `TestAddFileFuncOneReaderOpen` | No more than one reader is open at a time.            |
| `TestAddFileFuncMemory`        | Build 1 GiB of content, in 1024 files of 1 MiB each, from a reader that generates bytes. With compression off, `runtime.MemStats.TotalAlloc` increases by less than 64 MiB during `Build`. |
| `TestAddFromFSLazy`            | `AddFromFS` over an `fstest.MapFS` gives the same bytes as v0.6.1 gave for the same tree. Keep a golden file from v0.6.1. |

The memory test is the acceptance test for this spec. If it is slow, gate it
with `testing.Short`.

## Release

Release this change as v0.7.0. The API only grows, but `AddFromFS` has a new
failure mode, so a minor version is correct.

## The consumer change in objgit

After v0.7.0, objgit changes `snapshot.Ensure`:

- It calls `AddFileFunc` for each blob, with `blob.Size` and a function that
  returns `blob.Reader()`.
- It no longer reads blob content during the tree walk.

The objgit snapshot tests stay the same. `TestEnsureDeterministic` also
proves that the image bytes do not change when objgit moves to the new API.

## Later: compressed streaming

This section is for a later spec. It is not part of v0.7.0.

With compression on, the builder must compress a file before it knows the
layout of that file. One design that keeps memory bounded:

1. Before layout, open each lazy file and compress it one pcluster at a time.
2. Write the compressed output to a temp spool file.
3. Keep only the decision (compressed or flat), the index entries, and the
   extent sizes of the spool.
4. Do the layout from those sizes.
5. Copy each extent from the spool to its final offset.

If a file stays flat, open it a second time during the data step, as this
spec does. This design also calls `open` twice for such a file, so it changes
the "`open` runs once" rule.
