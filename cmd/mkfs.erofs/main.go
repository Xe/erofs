package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Xe/erofs"
	"github.com/spf13/pflag"
)

var (
	blockSize = pflag.Uint8("block-size", 12, "EROFS block size (power of two bytes), defaults to 4096 bytes")
	epoch     = pflag.Time("epoch", time.Unix(1, 1), []string{time.RFC3339}, "RFC3339 timestamp for the filesystem epoch")

	dir = pflag.StringP("dir", "i", "", "directory to pack into EROFS")
	out = pflag.StringP("out", "o", "", "resulting filesystem image name")
)

func main() {
	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage for %s: <--dir=in> <--out=./var/fs.raw> [options]\n\nFlags:\n", filepath.Base(os.Args[0]))

		pflag.CommandLine.PrintDefaults()
	}

	pflag.Parse()

	if *dir == "" || *out == "" {
		pflag.Usage()
		os.Exit(2)
	}

	fout, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "can't create %q: %v\n", *out, err)
		os.Exit(1)
	}
	defer fout.Close()

	b := erofs.NewBuilder(
		fout,
		erofs.WithBlockSize(*blockSize),
		erofs.WithEpoch(*epoch),
		erofs.WithCompression(erofs.CompressionAutoLZ4),
	)

	dirFS := os.DirFS(*dir)

	if err := b.AddFromFS(dirFS); err != nil {
		fmt.Fprintf(os.Stderr, "can't add %q: %v", *dir, err)
		os.Exit(1)
	}

	if err := b.Build(); err != nil {
		fmt.Fprintf(os.Stderr, "can't build filesystem: %v", err)
	}

	fmt.Println("wrote to", *out)
}
