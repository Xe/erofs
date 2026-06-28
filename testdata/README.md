# Instructions for generating test data

```
skopeo copy docker-daemon:docker.io/tianon/toybox:0.8.11 oci-archive:toybox.tar:docker.io/tianon/toybox:0.8.11
mkdir -p oci
tar -C oci -xf toybox.tar
sudo umoci unpack --image oci:docker.io/tianon/toybox:0.8.11 unpacked
sudo mkfs.erofs toybox.img unpacked/rootfs/
```

## Big-pcluster fixtures (issue #5)

Real big-pcluster images produced by `mkfs.erofs` 1.7.1, used to verify the
reader's big-pcluster decode path against reference tooling. Generated with the
legacy/FULL compressed index (`-E legacy-compress`) so they exercise the same
index format the builder emits:

```
mkfs.erofs -zlz4 -C65536 -b4096 -E legacy-compress bigpcluster-lz4-c1.img  <dir containing bigpcluster-lz4-c1.txt>
mkfs.erofs -zlz4 -C65536 -b4096 -E legacy-compress bigpcluster-lz4-c11.img <dir containing bigpcluster-lz4-c11.bin>
```

- `bigpcluster-lz4-c1.img` / `.txt` — 135000-byte highly compressible file that
  packs into a single physical block (compressed block count C=1, span K=33
  lclusters). Exercises the partial-tail boundary marker.
- `bigpcluster-lz4-c11.img` / `.bin` — 96839-byte semi-compressible file that
  packs into 11 physical blocks (C=11) in one pcluster. Exercises the
  `D0_CBLKCNT` compressed-block-count path (C > 1).

Both pass `fsck.erofs`.
