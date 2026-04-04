package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Xe/erofs"
	"github.com/spf13/pflag"
)

var (
	bind = pflag.StringP("bind", "b", ":9906", "TCP port to bind HTTP")
)

func main() {
	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage for %s: <--bind=:9906> <erofs-image>\n\nFlags:\n", filepath.Base(os.Args[0]))
		pflag.CommandLine.PrintDefaults()
	}

	pflag.Parse()

	if pflag.NArg() != 1 {
		pflag.Usage()
		os.Exit(2)
	}

	fname := pflag.Arg(0)

	if err := run(fname); err != nil {
		fmt.Fprintf(os.Stderr, "error serving %s: %v\n", fname, err)
	}
}

func run(fname string) error {
	fin, err := os.Open(fname)
	if err != nil {
		return fmt.Errorf("can't open file: %w", err)
	}
	defer fin.Close()

	fs, err := erofs.Open(fin)
	if err != nil {
		return fmt.Errorf("can't erofs.Open: %w", err)
	}

	http.Handle("/", http.FileServer(http.FS(fs)))
	log.Printf("Serving %s on %s", fname, *bind)
	return http.ListenAndServe(*bind, nil)
}
