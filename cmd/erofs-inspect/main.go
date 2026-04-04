package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Xe/erofs/internal/ondisk"
	"github.com/spf13/pflag"
)

func main() {
	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage for %s: <erofs-image>\n\nInspects an EROFS image and prints superblock info and inode layouts.\n\nFlags:\n", filepath.Base(os.Args[0]))
		pflag.CommandLine.PrintDefaults()
	}

	pflag.Parse()

	if pflag.NArg() != 1 {
		pflag.Usage()
		os.Exit(2)
	}

	f, err := os.Open(pflag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	// Read and decode superblock
	sbBuf := make([]byte, 144)
	if _, err := f.ReadAt(sbBuf, ondisk.SuperOffset); err != nil {
		fmt.Fprintf(os.Stderr, "reading superblock: %v\n", err)
		os.Exit(1)
	}
	var sb ondisk.SuperBlock
	if err := binary.Read(bytes.NewReader(sbBuf), binary.LittleEndian, &sb); err != nil {
		fmt.Fprintf(os.Stderr, "decoding superblock: %v\n", err)
		os.Exit(1)
	}

	blkSz := uint32(1) << sb.BlkSzBits
	fmt.Printf("Magic:            0x%08X\n", sb.Magic)
	fmt.Printf("BlkSzBits:        %d (block size: %d)\n", sb.BlkSzBits, blkSz)
	fmt.Printf("FeatureCompat:    0x%08X\n", sb.FeatureCompat)
	fmt.Printf("FeatureIncompat:  0x%08X\n", sb.FeatureIncompat)
	fmt.Printf("AvailComprAlgs:   0x%04X\n", sb.AvailComprAlgs)
	fmt.Printf("MetaBlkAddr:      %d\n", sb.MetaBlkAddr)
	fmt.Printf("RootNID2B:        %d\n", sb.RootNID2B)
	fmt.Printf("BlocksLo:         %d\n", sb.BlocksLo)
	fmt.Printf("Inos:             %d\n", sb.Inos)

	if sb.ExtraDevices > 0 {
		fmt.Printf("ExtraDevices:     %d\n", sb.ExtraDevices)
		fmt.Printf("DevtSlotOff:      %d (byte offset: %d)\n", sb.DevtSlotOff, int64(sb.DevtSlotOff)*128)

		devtOff := int64(sb.DevtSlotOff) * int64(ondisk.DevTSlotSize)
		for i := uint16(0); i < sb.ExtraDevices; i++ {
			slotBuf := make([]byte, ondisk.DevTSlotSize)
			if _, err := f.ReadAt(slotBuf, devtOff+int64(i)*int64(ondisk.DevTSlotSize)); err != nil {
				fmt.Fprintf(os.Stderr, "reading device slot %d: %v\n", i, err)
				continue
			}
			var slot ondisk.DeviceSlot
			binary.Read(bytes.NewReader(slotBuf), binary.LittleEndian, &slot)

			blocks := uint64(slot.BlocksLo) | uint64(slot.BlocksHi)<<32
			uniAddr := uint64(slot.UniAddrLo) | uint64(slot.UniAddrHi)<<32
			tag := bytes.TrimRight(slot.Tag[:], "\x00")
			fmt.Printf("  Device %d: blocks=%d uniaddr=%d tag=%q\n", i+1, blocks, uniAddr, tag)
		}
	}

	if sb.AvailComprAlgs&(1<<ondisk.CompressionLZ4) != 0 {
		fmt.Println("  -> LZ4 compression available")
	}

	// Scan inodes in metadata area
	metaOff := int64(sb.MetaBlkAddr) * int64(blkSz)
	fi, _ := f.Stat()
	endScan := min(fi.Size(), metaOff+1024*1024)

	fmt.Printf("\nScanning inodes from offset 0x%X...\n", metaOff)

	layoutNames := map[uint8]string{
		0: "FLAT_PLAIN",
		1: "COMPRESSED_FULL",
		2: "FLAT_INLINE",
		3: "COMPRESSED_COMPACT",
		4: "CHUNK_BASED",
	}
	layoutCounts := map[uint8]int{}
	totalInodes := 0

	for off := metaOff; off < endScan; off += ondisk.ISlotSize {
		var inoBuf [64]byte
		n, _ := f.ReadAt(inoBuf[:], off)
		if n < 32 {
			break
		}
		format := binary.LittleEndian.Uint16(inoBuf[0:2])
		if format&^uint16(ondisk.IAll) != 0 {
			continue
		}
		dataLayout := uint8((format >> ondisk.IDataLayoutBit) & ondisk.IDataLayoutMask)
		if dataLayout >= ondisk.InodeDataLayoutMax {
			continue
		}
		version := format & ondisk.IVersionMask
		mode := binary.LittleEndian.Uint16(inoBuf[4:6])

		modeType := mode & 0xF000
		if modeType != 0o100000 && modeType != 0o040000 && modeType != 0o120000 {
			continue
		}

		var size uint64
		switch {
		case version == ondisk.InodeLayoutCompact:
			size = uint64(binary.LittleEndian.Uint32(inoBuf[8:12]))
		case n >= 64:
			size = binary.LittleEndian.Uint64(inoBuf[8:16])
		}

		nid := uint64(off-metaOff) >> ondisk.ISlotBits

		layoutCounts[dataLayout]++
		totalInodes++

		var typeStr string
		switch modeType {
		case 0o040000:
			typeStr = "dir"
		case 0o120000:
			typeStr = "link"
		default:
			typeStr = "file"
		}

		fmt.Printf("  nid=%-4d %-5s layout=%-20s size=%d\n", nid, typeStr, layoutNames[dataLayout], size)

		if version == ondisk.InodeLayoutExtended {
			off += ondisk.ISlotSize
		}
	}

	fmt.Printf("\n--- Layout summary (%d inodes found) ---\n", totalInodes)
	for layout, count := range layoutCounts {
		fmt.Printf("  %-20s: %d\n", layoutNames[layout], count)
	}
}
