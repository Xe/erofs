# erofs

Pure-Go [EROFS](https://erofs.docs.kernel.org/en/latest/) (Enhanced Read-Only
File System) reader and writer. Read EROFS images through Go's `fs.FS`
interface, or create new ones with `Builder`. Output is bytewise compatible with
the Linux kernel EROFS driver and `mkfs.erofs`.

Reads LZ4, LZMA, DEFLATE, and Zstandard compressed images. Writes LZ4
compressed images with automatic incompressibility detection.

## Install

```sh
go get github.com/Xe/erofs@latest
```

## Reading an EROFS image

`erofs.Open` accepts any `io.ReaderAt` and returns an `*erofs.FS` implementing
`fs.FS`, `fs.StatFS`, and `fs.ReadLinkFS`:

```go
package main

import (
    "fmt"
    "io/fs"
    "log"
    "os"

    "github.com/Xe/erofs"
)

func main() {
    f, err := os.Open("rootfs.erofs")
    if err != nil {
        log.Fatal(err)
    }
    defer f.Close()

    fsys, err := erofs.Open(f)
    if err != nil {
        log.Fatal(err)
    }

    // Read a file.
    data, err := fs.ReadFile(fsys, "etc/hostname")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(string(data))

    // List a directory.
    entries, err := fs.ReadDir(fsys, "usr/bin")
    if err != nil {
        log.Fatal(err)
    }
    for _, e := range entries {
        fmt.Println(e.Name())
    }

    // Stat a file.
    info, err := fsys.Stat("etc/passwd")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("%s: %d bytes\n", info.Name(), info.Size())

    // Read a symlink.
    target, err := fsys.ReadLink("usr/bin/vi")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("vi ->", target)
}
```

## Creating an EROFS image

`erofs.NewBuilder` writes to any `io.WriterAt`. Add files individually or walk
an existing `fs.FS`:

```go
package main

import (
    "log"
    "os"
    "time"

    "github.com/Xe/erofs"
)

func main() {
    out, err := os.Create("output.erofs")
    if err != nil {
        log.Fatal(err)
    }
    defer out.Close()

    b := erofs.NewBuilder(out,
        erofs.WithBlockSize(12), // 4096-byte blocks
        erofs.WithEpoch(time.Now()),
        erofs.WithCompression(erofs.CompressionAutoLZ4),
    )

    // Add from an existing directory tree.
    dirFS := os.DirFS("/path/to/rootfs")
    if err := b.AddFromFS(dirFS); err != nil {
        log.Fatal(err)
    }

    if err := b.Build(); err != nil {
        log.Fatal(err)
    }
}
```

Or add entries one at a time:

```go
b := erofs.NewBuilder(out, erofs.WithEpoch(time.Now()))

b.AddDir("/etc", dirInfo)
b.AddFile("/etc/hostname", fileInfo, []byte("myhost\n"))
b.AddSymlink("/etc/localtime", "/usr/share/zoneinfo/UTC", linkInfo)

if err := b.Build(); err != nil {
    log.Fatal(err)
}
```

## CLI tools

Install all three:

```sh
go install github.com/Xe/erofs/cmd/...@latest
```

### mkfs.erofs

Create an EROFS image from a directory:

```sh
mkfs.erofs --dir ./rootfs --out rootfs.erofs
mkfs.erofs --dir ./rootfs --out rootfs.erofs --epoch 2025-01-01T00:00:00Z
```

### erofs-inspect

Print superblock metadata and inode statistics:

```sh
erofs-inspect rootfs.erofs
```

### erofs-serve

Serve an EROFS image over HTTP:

```sh
erofs-serve --bind :8080 rootfs.erofs
```

## On-disk format

Follows the [EROFS on-disk format
specification](https://erofs.docs.kernel.org/en/latest/). The `internal/ondisk`
package maps all binary structures (`SuperBlock`, `InodeCompact`,
`InodeExtended`, `Dirent`, etc.) to the kernel's `erofs_fs.h`.

Format details:

- Superblock at offset 1024, magic `0xE0F5E1E2`
- CRC32-C checksums (seed `0x5045B54A`)
- 32-byte compact and 64-byte extended inode formats
- Flat plain, flat inline, and compressed data layouts
- 12-byte directory entries: 8-byte NID, 2-byte name offset, 1-byte file type

The `kernel/` directory (untracked) holds a copy of the Linux kernel EROFS
driver source, used as a specification reference during development.

## Development

```sh
go build ./...
go test ./...
```

Pre-commit hooks run `goimports` and the test suite. Install with `npm ci`.