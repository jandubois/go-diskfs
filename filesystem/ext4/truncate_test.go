package ext4

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
)

// testTruncateFS creates a filesystem in a fresh image and writes filename into it,
// filling blocks filesystem blocks with random data, written chunkBlocks blocks at a
// time. It returns the image path, the file backing it, the filesystem and the data.
func testTruncateFS(t *testing.T, filename string, blocks, chunkBlocks int) (outfile string, backing *os.File, fs *FileSystem, data []byte) {
	t.Helper()
	outfile, backing = testCreateEmptyFile(t, 100*MB)
	t.Cleanup(func() { _ = backing.Close() })

	fs, err := Create(file.New(backing, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	size := blocks * int(fs.superblock.blockSize)
	chunk := chunkBlocks * int(fs.superblock.blockSize)
	data = make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read failed: %v", err)
	}
	fl, err := fs.OpenFile(filename, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	for offset := 0; offset < size; offset += chunk {
		end := min(offset+chunk, size)
		if _, err := fl.Write(data[offset:end]); err != nil {
			t.Fatalf("Write at offset %d failed: %v", offset, err)
		}
	}
	return outfile, backing, fs, data
}

// testFsck flushes the image to disk and checks it with e2fsck.
func testFsck(t *testing.T, f *os.File, outfile string) {
	t.Helper()
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	out, err := exec.Command("e2fsck", "-f", "-n", outfile).CombinedOutput()
	if err != nil {
		t.Errorf("e2fsck failed: %v\n%s", err, string(out))
	}
}

// testFileBlocks returns how many filesystem blocks the extents of filename cover.
func testFileBlocks(t *testing.T, fs *FileSystem, filename string) uint64 {
	t.Helper()
	exts, err := testFileInode(t, fs, filename).extents.blocks(fs)
	if err != nil {
		t.Fatalf("could not read extents of %s: %v", filename, err)
	}
	return exts.blockCount()
}

// testFileInode returns the inode of filename.
func testFileInode(t *testing.T, fs *FileSystem, filename string) *inode {
	t.Helper()
	_, entry, err := fs.getEntryAndParent(filename)
	if err != nil {
		t.Fatalf("could not look up %s: %v", filename, err)
	}
	in, err := fs.readInode(entry.inode)
	if err != nil {
		t.Fatalf("could not read inode %d: %v", entry.inode, err)
	}
	return in
}

// testReadFile returns the first n bytes of filename.
func testReadFile(t *testing.T, fs *FileSystem, filename string, n int) []byte {
	t.Helper()
	fl, err := fs.OpenFile(filename, os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	content := make([]byte, n)
	if _, err := io.ReadFull(fl, content); err != nil {
		t.Fatalf("reading %d bytes of %s failed: %v", n, filename, err)
	}
	return content
}

// TestTruncate checks that shrinking a file frees the blocks past the new size,
// that growing one leaves a hole, and that the data below the new size survives.
func TestTruncate(t *testing.T) {
	const originalBlocks = 10
	tests := []struct {
		name string
		// the size to truncate to, as whole blocks plus a remainder in bytes
		blocks    int64
		remainder int64
		// how many blocks the file covers afterwards
		kept uint64
	}{
		{"to a block boundary", 1, 0, 1},
		{"into the middle of a block", 1, 100, 2},
		{"to zero", 0, 0, 0},
		{"to the size it already is", originalBlocks, 0, originalBlocks},
		{"past the end of the file", 2 * originalBlocks, 0, originalBlocks},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outfile, f, fs, data := testTruncateFS(t, "/big.dat", originalBlocks, originalBlocks)
			blockSize := int64(fs.superblock.blockSize)
			size := tt.blocks*blockSize + tt.remainder
			freeBlocksBefore := fs.superblock.freeBlocks

			if err := fs.Truncate("/big.dat", size); err != nil {
				t.Fatalf("Truncate failed: %v", err)
			}

			fi, err := fs.Stat("/big.dat")
			if err != nil {
				t.Fatalf("Stat failed: %v", err)
			}
			if fi.Size() != size {
				t.Errorf("file size is %d, expected %d", fi.Size(), size)
			}
			if blocks := testFileBlocks(t, fs, "/big.dat"); blocks != tt.kept {
				t.Errorf("file covers %d blocks, expected %d", blocks, tt.kept)
			}
			if freed := fs.superblock.freeBlocks - freeBlocksBefore; freed != originalBlocks-tt.kept {
				t.Errorf("truncating freed %d blocks, expected %d", freed, originalBlocks-tt.kept)
			}

			survives := min(int(size), len(data))
			if content := testReadFile(t, fs, "/big.dat", survives); !bytes.Equal(content, data[:survives]) {
				t.Errorf("content below the new size does not match what was written")
			}
			testFsck(t, f, outfile)
		})
	}
}

// TestOpenFileTruncate checks that os.O_TRUNC empties the file it opens.
func TestOpenFileTruncate(t *testing.T) {
	const originalBlocks = 10
	outfile, f, fs, _ := testTruncateFS(t, "/big.dat", originalBlocks, originalBlocks)
	freeBlocksBefore := fs.superblock.freeBlocks

	fl, err := fs.OpenFile("/big.dat", os.O_RDWR|os.O_TRUNC)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	replacement := []byte("hello")
	if _, err := fl.Write(replacement); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	fi, err := fs.Stat("/big.dat")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if fi.Size() != int64(len(replacement)) {
		t.Errorf("file size is %d, expected %d", fi.Size(), len(replacement))
	}
	if content := testReadFile(t, fs, "/big.dat", len(replacement)); !bytes.Equal(content, replacement) {
		t.Errorf("file holds %q, expected %q", content, replacement)
	}
	// every original block is freed, and one is taken back for the replacement
	if freed := fs.superblock.freeBlocks - freeBlocksBefore; freed != originalBlocks-1 {
		t.Errorf("truncating freed %d blocks, expected %d", freed, originalBlocks-1)
	}
	testFsck(t, f, outfile)
}

// TestOpenFileAppendTruncate checks that os.O_APPEND alongside os.O_TRUNC starts
// writing at the beginning of the emptied file rather than at its former end.
func TestOpenFileAppendTruncate(t *testing.T) {
	outfile, f, fs, _ := testTruncateFS(t, "/big.dat", 10, 10)

	fl, err := fs.OpenFile("/big.dat", os.O_RDWR|os.O_APPEND|os.O_TRUNC)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	replacement := []byte("hello")
	if _, err := fl.Write(replacement); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	fi, err := fs.Stat("/big.dat")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if fi.Size() != int64(len(replacement)) {
		t.Errorf("file size is %d, expected %d", fi.Size(), len(replacement))
	}
	testFsck(t, f, outfile)
}

// TestTruncateExtentTreeWithInternalNodes checks that truncating a file whose extent
// tree outgrew the inode is refused, rather than leaking the blocks the tree lives in.
func TestTruncateExtentTreeWithInternalNodes(t *testing.T) {
	// written in chunks, the file fragments into more than the four extents the inode
	// holds, so its extent tree gains internal nodes
	outfile, f, fs, _ := testTruncateFS(t, "/large.dat", 1024, 32)

	if depth := testFileInode(t, fs, "/large.dat").extents.getDepth(); depth == 0 {
		t.Fatalf("extent tree of /large.dat has no internal nodes, nothing to refuse")
	}

	err := fs.Truncate("/large.dat", 4096)
	if err == nil {
		t.Fatalf("Truncate accepted a file whose extent tree has internal nodes")
	}
	if !strings.Contains(err.Error(), "internal nodes") {
		t.Errorf("Truncate failed with %v, expected the error to name internal nodes", err)
	}
	// the file it refused to truncate is left whole
	testFsck(t, f, outfile)
}

// TestTruncateNegativeSize checks that a negative size is refused, rather than
// becoming an enormous one.
func TestTruncateNegativeSize(t *testing.T) {
	const originalBlocks = 10
	_, _, fs, data := testTruncateFS(t, "/big.dat", originalBlocks, originalBlocks)

	if err := fs.Truncate("/big.dat", -1); err == nil {
		t.Fatalf("Truncate accepted a negative size")
	}
	fi, err := fs.Stat("/big.dat")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if fi.Size() != int64(len(data)) {
		t.Errorf("file size is %d, expected it to be left at %d", fi.Size(), len(data))
	}
}

// TestOpenFileTruncateDirectory checks that os.O_TRUNC on a directory is refused,
// rather than freeing the blocks its entries live in.
func TestOpenFileTruncateDirectory(t *testing.T) {
	outfile, f := testCreateEmptyFile(t, 100*MB)
	t.Cleanup(func() { _ = f.Close() })

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if err := fs.Mkdir("adir"); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	inner, err := fs.OpenFile("/adir/inner.txt", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if _, err := inner.Write([]byte("hello")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if _, err := fs.OpenFile("/adir", os.O_RDWR|os.O_TRUNC); err == nil {
		t.Fatalf("OpenFile truncated a directory")
	}

	entries, err := fs.ReadDir("adir")
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Name() == "inner.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("adir lost inner.txt, it holds %d entries", len(entries))
	}
	testFsck(t, f, outfile)
}

// TestTruncateZeroesTail checks that shrinking a file into the middle of a block
// zeroes the rest of that block, so growing the file again reads zeros rather than
// the data that was there before.
func TestTruncateZeroesTail(t *testing.T) {
	const keep = 100
	outfile, f, fs, data := testTruncateFS(t, "/big.dat", 2, 2)
	blockSize := int(fs.superblock.blockSize)

	if err := fs.Truncate("/big.dat", keep); err != nil {
		t.Fatalf("Truncate to %d failed: %v", keep, err)
	}
	if err := fs.Truncate("/big.dat", int64(blockSize)); err != nil {
		t.Fatalf("Truncate to %d failed: %v", blockSize, err)
	}

	content := testReadFile(t, fs, "/big.dat", blockSize)
	if !bytes.Equal(content[:keep], data[:keep]) {
		t.Errorf("the data below the truncated size changed")
	}
	for i := keep; i < blockSize; i++ {
		if content[i] != 0 {
			t.Fatalf("byte %d past the truncated size is %#x, expected 0", i, content[i])
		}
	}
	testFsck(t, f, outfile)
}

// TestOpenFileTruncateReadOnly checks that os.O_TRUNC needs write intent. os.O_RDONLY
// is 0, so the flag on its own must leave the file alone.
func TestOpenFileTruncateReadOnly(t *testing.T) {
	const originalBlocks = 10
	outfile, f, fs, data := testTruncateFS(t, "/big.dat", originalBlocks, originalBlocks)

	if _, err := fs.OpenFile("/big.dat", os.O_RDONLY|os.O_TRUNC); err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}

	fi, err := fs.Stat("/big.dat")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if fi.Size() != int64(len(data)) {
		t.Errorf("file size is %d, expected it to be left at %d", fi.Size(), len(data))
	}
	if blocks := testFileBlocks(t, fs, "/big.dat"); blocks != originalBlocks {
		t.Errorf("file covers %d blocks, expected it to keep %d", blocks, originalBlocks)
	}
	testFsck(t, f, outfile)
}

// TestTruncateUninitializedExtent checks that an extent carrying the uninitialized
// flag is refused. ext4 marks one by setting bit 15 of ee_len, which the extent
// parser does not mask, so such an extent reads as 32768 blocks longer than it is
// and freeing its tail would free blocks belonging to other files.
func TestTruncateUninitializedExtent(t *testing.T) {
	outfile, f, fs, _ := testTruncateFS(t, "/big.dat", 4, 4)

	in := testFileInode(t, fs, "/big.dat")
	existing, err := in.extents.blocks(fs)
	if err != nil {
		t.Fatalf("could not read extents: %v", err)
	}
	if len(existing) == 0 {
		t.Fatalf("/big.dat has no extents")
	}
	marked := make(extents, len(existing))
	copy(marked, existing)
	marked[0].count += maxBlocksPerExtent
	in.extents = extentsBlockFinderFromExtents(marked, fs.superblock.blockSize)

	// one block, well below the four the file holds, so this shrinks it
	err = fs.truncateInode(in, uint64(fs.superblock.blockSize))
	if err == nil {
		t.Fatalf("truncateInode accepted an uninitialized extent")
	}
	if !strings.Contains(err.Error(), "uninitialized") {
		t.Errorf("truncateInode failed with %v, expected the error to name the uninitialized extent", err)
	}
	// the file it refused to truncate is left whole
	testFsck(t, f, outfile)
}

// TestTruncateKeepsExtendedAttributeBlock checks that i_blocks still counts an
// external extended attribute block after a truncation. Such a block comes from an
// image built elsewhere, so this fabricates one on the inode rather than creating it,
// and asserts on the count alone: the block it names holds no attributes, so the
// image it leaves behind is not one to hand to e2fsck.
func TestTruncateKeepsExtendedAttributeBlock(t *testing.T) {
	const originalBlocks = 4
	_, _, fs, data := testTruncateFS(t, "/big.dat", originalBlocks, originalBlocks)
	blockSize := uint64(fs.superblock.blockSize)

	in := testFileInode(t, fs, "/big.dat")
	if in.size != uint64(len(data)) {
		t.Fatalf("the file is %d bytes, expected the %d written to it", in.size, len(data))
	}
	in.extendedAttributeBlock = 1234

	if err := fs.truncateInode(in, blockSize); err != nil {
		t.Fatalf("truncateInode failed: %v", err)
	}

	// one data block left, plus the attribute block
	expected := uint64(2)
	if !in.filesystemBlocks {
		expected = expected * blockSize / 512
	}
	if in.blocks != expected {
		t.Errorf("i_blocks is %d, expected %d", in.blocks, expected)
	}
}

// TestReadHole checks that reading a hole returns zeros. Shrinking a file and growing
// it again leaves file blocks with no extent behind them, and a read that matches no
// extent returned no bytes and no error, which spins io.ReadAll forever.
func TestReadHole(t *testing.T) {
	const originalBlocks = 4
	outfile, f, fs, data := testTruncateFS(t, "/hole.dat", originalBlocks, originalBlocks)
	blockSize := int64(fs.superblock.blockSize)

	if err := fs.Truncate("/hole.dat", blockSize); err != nil {
		t.Fatalf("Truncate to one block failed: %v", err)
	}
	if err := fs.Truncate("/hole.dat", 3*blockSize); err != nil {
		t.Fatalf("Truncate to three blocks failed: %v", err)
	}

	fl, err := fs.OpenFile("/hole.dat", os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	content := make([]byte, 3*blockSize)
	read := 0
	for read < len(content) {
		n, err := fl.Read(content[read:])
		read += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read at offset %d failed: %v", read, err)
		}
		if n == 0 {
			t.Fatalf("Read returned no bytes and no error at offset %d of %d", read, len(content))
		}
	}
	if read != len(content) {
		t.Fatalf("read %d bytes of %s, expected %d", read, "/hole.dat", len(content))
	}
	if !bytes.Equal(content[:blockSize], data[:blockSize]) {
		t.Error("the block below the truncation point lost its data")
	}
	if !bytes.Equal(content[blockSize:], make([]byte, 2*blockSize)) {
		t.Error("the hole read as something other than zeros")
	}
	testFsck(t, f, outfile)
}

// TestOpenFileTruncateWriteOnly checks that os.O_TRUNC leaves the file alone when the
// handle cannot write it. This package writes through os.O_RDWR alone, so emptying an
// os.O_WRONLY file would leave nothing able to put the contents back.
func TestOpenFileTruncateWriteOnly(t *testing.T) {
	const originalBlocks = 10
	outfile, f, fs, data := testTruncateFS(t, "/big.dat", originalBlocks, originalBlocks)

	if _, err := fs.OpenFile("/big.dat", os.O_WRONLY|os.O_TRUNC); err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}

	fi, err := fs.Stat("/big.dat")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if fi.Size() != int64(len(data)) {
		t.Errorf("file size is %d, expected it to be left at %d", fi.Size(), len(data))
	}
	if blocks := testFileBlocks(t, fs, "/big.dat"); blocks != originalBlocks {
		t.Errorf("file covers %d blocks, expected it to keep %d", blocks, originalBlocks)
	}
	testFsck(t, f, outfile)
}

// TestTruncateSymlink checks that truncating a symlink is refused. A target too long
// for the inode lives in a block of its own, and freeing that block would hand it to
// the next file while the symlink still points at it.
func TestTruncateSymlink(t *testing.T) {
	outfile, f := testCreateEmptyFile(t, 100*MB)
	t.Cleanup(func() { _ = f.Close() })
	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	target := strings.Repeat("t", 84)
	if err := fs.Symlink(target, "link"); err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	if err := fs.Truncate("/link", 0); err == nil {
		t.Fatal("truncating a symlink was allowed")
	}

	got, err := fs.ReadLink("link")
	if err != nil {
		t.Fatalf("ReadLink failed: %v", err)
	}
	if got != target {
		t.Errorf("the symlink points at %q, expected %q", got, target)
	}
	testFsck(t, f, outfile)
}

// TestDeallocateExtentsAtGroupBoundary checks that freeing the first block of a block
// group clears the bit for that block. A filesystem with 4 KiB blocks starts at block
// 0, and deriving the group from the block number alone then puts that block in the
// group before it, past the end of its bitmap.
func TestDeallocateExtentsAtGroupBoundary(t *testing.T) {
	const size = 1024 * MB
	_, f := testCreateEmptyFile(t, size)
	t.Cleanup(func() { _ = f.Close() })
	fs, err := Create(file.New(f, false), size, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if fs.superblock.firstDataBlock != 0 {
		t.Fatalf("the filesystem starts at block %d, expected 4 KiB blocks to start at 0", fs.superblock.firstDataBlock)
	}
	boundary := uint64(fs.superblock.blocksPerGroup)

	bm, err := fs.readBlockBitmap(1)
	if err != nil {
		t.Fatalf("could not read the block bitmap of group 1: %v", err)
	}
	if err := bm.Set(0); err != nil {
		t.Fatalf("could not mark block %d used: %v", boundary, err)
	}
	if err := fs.writeBlockBitmap(bm, 1); err != nil {
		t.Fatalf("could not write the block bitmap of group 1: %v", err)
	}

	if err := fs.deallocateExtents(extents{{startingBlock: boundary, count: 1}}); err != nil {
		t.Fatalf("deallocateExtents failed: %v", err)
	}

	bm, err = fs.readBlockBitmap(1)
	if err != nil {
		t.Fatalf("could not read the block bitmap of group 1 back: %v", err)
	}
	if used, err := bm.IsSet(0); err != nil {
		t.Fatalf("could not read the bit for block %d: %v", boundary, err)
	} else if used {
		t.Errorf("block %d is still marked used", boundary)
	}
}
