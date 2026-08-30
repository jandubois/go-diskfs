package ext4

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
)

// TestWriteMultiBlock writes data larger than one filesystem block (4KB default)
// and verifies it can be read back correctly.
func TestWriteMultiBlock(t *testing.T) {
	sizes := []struct {
		name string
		size int
	}{
		{"exactly 1 block (4096)", 4096},
		{"just over 1 block (4097)", 4097},
		{"2 blocks (8192)", 8192},
		{"5 blocks (20480)", 20480},
		{"10 blocks (40960)", 40960},
		{"partial last block (6000)", 6000},
	}

	for _, sz := range sizes {
		t.Run(sz.name, func(t *testing.T) {
			outfile, f := testCreateEmptyFile(t, 100*MB)
			defer f.Close()

			fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
			if err != nil {
				t.Fatalf("Create failed: %v", err)
			}

			// Generate random data of the desired size
			data := make([]byte, sz.size)
			if _, err := rand.Read(data); err != nil {
				t.Fatalf("rand.Read failed: %v", err)
			}

			// Write the file
			ext4File, err := fs.OpenFile("/bigfile.dat", os.O_CREATE|os.O_RDWR)
			if err != nil {
				t.Fatalf("OpenFile failed: %v", err)
			}
			n, err := ext4File.Write(data)
			if err != nil {
				t.Fatalf("Write failed: %v", err)
			}
			if n != sz.size {
				t.Fatalf("short write: expected %d, got %d", sz.size, n)
			}

			// Seek back to beginning and read
			if _, err := ext4File.Seek(0, io.SeekStart); err != nil {
				t.Fatalf("Seek failed: %v", err)
			}
			readBuf := make([]byte, sz.size)
			nRead, err := ext4File.Read(readBuf)
			if err != nil && err != io.EOF {
				t.Fatalf("Read failed: %v", err)
			}
			if nRead != sz.size {
				t.Fatalf("short read: expected %d, got %d", sz.size, nRead)
			}
			if !bytes.Equal(data, readBuf) {
				t.Errorf("data mismatch after write/read of %d bytes", sz.size)
			}

			// Validate with e2fsck
			if err := f.Sync(); err != nil {
				t.Fatalf("Sync failed: %v", err)
			}
			cmd := exec.Command("e2fsck", "-f", "-n", outfile)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("e2fsck failed: %v\n%s", err, string(out))
			}
		})
	}
}

// TestWriteLargeFile writes a 1MB file using chunked 32KB writes. This exercises
// the extent tree expansion code path (loadChildNode, extendInternalNode) since
// 32KB chunks can produce non-contiguous extents that exceed the root node's
// 4-extent limit, forcing tree depth > 0.
func TestWriteLargeFile(t *testing.T) {
	outfile, f := testCreateEmptyFile(t, 200*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 200*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Write a 1MB file in 32KB chunks to force multiple extents.
	totalSize := int(1 * MB)
	chunkSize := 32 * 1024
	data := make([]byte, totalSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read failed: %v", err)
	}

	ext4File, err := fs.OpenFile("/largefile.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}

	// Write in chunks
	for offset := 0; offset < totalSize; offset += chunkSize {
		end := offset + chunkSize
		if end > totalSize {
			end = totalSize
		}
		n, err := ext4File.Write(data[offset:end])
		if err != nil {
			t.Fatalf("Write chunk at offset %d failed: %v", offset, err)
		}
		if n != end-offset {
			t.Fatalf("short write at offset %d: expected %d, got %d", offset, end-offset, n)
		}
	}

	// Seek back and verify
	if _, err := ext4File.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	readBuf := make([]byte, totalSize)
	totalRead := 0
	for totalRead < totalSize {
		nr, err := ext4File.Read(readBuf[totalRead:])
		if nr > 0 {
			totalRead += nr
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read failed at offset %d: %v", totalRead, err)
		}
	}
	if totalRead != totalSize {
		t.Fatalf("total read %d != expected %d", totalRead, totalSize)
	}
	if !bytes.Equal(data, readBuf) {
		for i := range data {
			if data[i] != readBuf[i] {
				t.Errorf("data mismatch at byte %d: wrote 0x%02x, read 0x%02x", i, data[i], readBuf[i])
				break
			}
		}
	}

	// Validate with e2fsck
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	cmd := exec.Command("e2fsck", "-f", "-n", outfile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("e2fsck failed: %v\n%s", err, string(out))
	}
}

// TestWriteMultipleFiles writes several files and verifies they can all be read back
func TestWriteMultipleFiles(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	files := map[string][]byte{
		"/file1.dat": make([]byte, 1024),
		"/file2.dat": make([]byte, 8192),
		"/file3.dat": make([]byte, 50000),
	}

	// Fill with random data and write
	for path, data := range files {
		if _, err := rand.Read(data); err != nil {
			t.Fatalf("rand.Read failed: %v", err)
		}
		ext4File, err := fs.OpenFile(path, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatalf("OpenFile %s failed: %v", path, err)
		}
		n, err := ext4File.Write(data)
		if err != nil {
			t.Fatalf("Write %s failed: %v", path, err)
		}
		if n != len(data) {
			t.Fatalf("short write on %s: expected %d, got %d", path, len(data), n)
		}
	}

	// Read back each file and verify
	for path, expected := range files {
		ext4File, err := fs.OpenFile(path, os.O_RDONLY)
		if err != nil {
			t.Fatalf("OpenFile %s for read failed: %v", path, err)
		}
		readBuf := make([]byte, len(expected))
		n, err := ext4File.Read(readBuf)
		if err != nil && err != io.EOF {
			t.Fatalf("Read %s failed: %v", path, err)
		}
		if n != len(expected) {
			t.Errorf("short read on %s: expected %d, got %d", path, len(expected), n)
		}
		if !bytes.Equal(expected, readBuf[:n]) {
			t.Errorf("data mismatch on %s", path)
		}
	}
}

// TestWriteSeekAndOverwrite writes data, seeks to an earlier position, and overwrites part of it
func TestWriteSeekAndOverwrite(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Write initial data
	initial := bytes.Repeat([]byte("AAAA"), 2048) // 8KB of 'A's
	ext4File, err := fs.OpenFile("/overwrite.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if _, err := ext4File.Write(initial); err != nil {
		t.Fatalf("initial Write failed: %v", err)
	}

	// Seek to offset 1024 and overwrite with 'B's
	overwriteOffset := int64(1024)
	overwriteData := bytes.Repeat([]byte("B"), 2048)
	if _, err := ext4File.Seek(overwriteOffset, io.SeekStart); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	if _, err := ext4File.Write(overwriteData); err != nil {
		t.Fatalf("overwrite Write failed: %v", err)
	}

	// Build expected result
	expected := make([]byte, len(initial))
	copy(expected, initial)
	copy(expected[overwriteOffset:], overwriteData)

	// Seek back and verify
	if _, err := ext4File.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek to start failed: %v", err)
	}
	readBuf := make([]byte, len(expected))
	n, err := ext4File.Read(readBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(expected) {
		t.Fatalf("short read: expected %d, got %d", len(expected), n)
	}
	if !bytes.Equal(expected, readBuf) {
		// Find first mismatch
		for i := range expected {
			if expected[i] != readBuf[i] {
				t.Errorf("data mismatch at byte %d: expected 0x%02x, got 0x%02x", i, expected[i], readBuf[i])
				break
			}
		}
	}
}

// TestWriteZeroLength verifies that a zero-length write does not error
func TestWriteZeroLength(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	ext4File, err := fs.OpenFile("/empty.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}

	n, err := ext4File.Write([]byte{})
	if err != nil {
		t.Fatalf("zero-length Write failed: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 bytes written, got %d", n)
	}
}

// TestWriteReadOnly verifies that writing to a read-only file returns an error
func TestWriteReadOnly(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Create a file first
	ext4File, err := fs.OpenFile("/readonly.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile for create failed: %v", err)
	}
	if _, err := ext4File.Write([]byte("hello")); err != nil {
		t.Fatalf("initial Write failed: %v", err)
	}

	// Open read-only
	roFile, err := fs.OpenFile("/readonly.dat", os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile read-only failed: %v", err)
	}
	_, err = roFile.Write([]byte("world"))
	if err == nil {
		t.Errorf("expected error writing to read-only file, got nil")
	}
}

// TestWriteAppend writes data, then opens the file in append mode and writes more
func TestWriteAppend(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	firstData := []byte("Hello, ")
	secondData := []byte("World!")

	// Write initial data
	ext4File, err := fs.OpenFile("/append.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if _, err := ext4File.Write(firstData); err != nil {
		t.Fatalf("first Write failed: %v", err)
	}

	// Open in append mode and write more
	appendFile, err := fs.OpenFile("/append.dat", os.O_APPEND|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile append failed: %v", err)
	}
	if _, err := appendFile.Write(secondData); err != nil {
		t.Fatalf("append Write failed: %v", err)
	}

	// Read back and verify
	readFile, err := fs.OpenFile("/append.dat", os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile for read failed: %v", err)
	}
	expected := firstData
	expected = append(expected, secondData...)
	readBuf := make([]byte, len(expected)+10)
	n, err := readFile.Read(readBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(expected) {
		t.Fatalf("expected %d bytes, got %d", len(expected), n)
	}
	if !bytes.Equal(expected, readBuf[:n]) {
		t.Errorf("data mismatch: expected %q, got %q", string(expected), string(readBuf[:n]))
	}
}

// TestWriteInSubdirectory writes a file in a subdirectory and verifies the round trip
func TestWriteInSubdirectory(t *testing.T) {
	outfile, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Create subdirectory
	if err := fs.Mkdir("subdir"); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	data := make([]byte, 16384) // 4 blocks
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read failed: %v", err)
	}

	// Write file in subdirectory
	ext4File, err := fs.OpenFile("/subdir/data.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if _, err := ext4File.Write(data); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Read back
	if _, err := ext4File.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	readBuf := make([]byte, len(data))
	n, err := ext4File.Read(readBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("short read: expected %d, got %d", len(data), n)
	}
	if !bytes.Equal(data, readBuf) {
		t.Errorf("data mismatch in subdirectory file")
	}

	// Validate with e2fsck
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	cmd := exec.Command("e2fsck", "-f", "-n", outfile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("e2fsck failed: %v\n%s", err, string(out))
	}
}

// TestWriteSeekPastEOF writes data, seeks past the end, and writes more data.
// The gap should be implicitly zero-filled.
func TestWriteSeekPastEOF(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	ext4File, err := fs.OpenFile("/sparse.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}

	// Write first chunk
	firstData := []byte("START")
	if _, err := ext4File.Write(firstData); err != nil {
		t.Fatalf("first Write failed: %v", err)
	}

	// Seek to well past the end
	gapOffset := int64(8192)
	if _, err := ext4File.Seek(gapOffset, io.SeekStart); err != nil {
		t.Fatalf("Seek past EOF failed: %v", err)
	}

	// Write after the gap
	secondData := []byte("END")
	if _, err := ext4File.Write(secondData); err != nil {
		t.Fatalf("second Write failed: %v", err)
	}

	// Read back: first chunk should be at offset 0
	if _, err := ext4File.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek to start failed: %v", err)
	}
	readBuf := make([]byte, len(firstData))
	n, err := ext4File.Read(readBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read first chunk failed: %v", err)
	}
	if n != len(firstData) {
		t.Fatalf("short read of first chunk: expected %d, got %d", len(firstData), n)
	}
	if !bytes.Equal(firstData, readBuf[:n]) {
		t.Errorf("first chunk mismatch: expected %q, got %q", string(firstData), string(readBuf[:n]))
	}

	// Read at the gap offset: should get the second data
	if _, err := ext4File.Seek(gapOffset, io.SeekStart); err != nil {
		t.Fatalf("Seek to gap offset failed: %v", err)
	}
	readBuf2 := make([]byte, len(secondData))
	n, err = ext4File.Read(readBuf2)
	if err != nil && err != io.EOF {
		t.Fatalf("Read second chunk failed: %v", err)
	}
	if n != len(secondData) {
		t.Fatalf("short read of second chunk: expected %d, got %d", len(secondData), n)
	}
	if !bytes.Equal(secondData, readBuf2[:n]) {
		t.Errorf("second chunk mismatch: expected %q, got %q", string(secondData), string(readBuf2[:n]))
	}
}

// TestSeekWhenceVariants tests all three Seek whence modes
func TestSeekWhenceVariants(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	ext4File, err := fs.OpenFile("/seektest.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}

	data := make([]byte, 1024)
	if _, err := ext4File.Write(data); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// SeekStart
	pos, err := ext4File.Seek(100, io.SeekStart)
	if err != nil {
		t.Fatalf("SeekStart failed: %v", err)
	}
	if pos != 100 {
		t.Errorf("SeekStart: expected position 100, got %d", pos)
	}

	// SeekCurrent
	pos, err = ext4File.Seek(50, io.SeekCurrent)
	if err != nil {
		t.Fatalf("SeekCurrent failed: %v", err)
	}
	if pos != 150 {
		t.Errorf("SeekCurrent: expected position 150, got %d", pos)
	}

	// SeekEnd
	pos, err = ext4File.Seek(-100, io.SeekEnd)
	if err != nil {
		t.Fatalf("SeekEnd failed: %v", err)
	}
	if pos != int64(len(data))-100 {
		t.Errorf("SeekEnd: expected position %d, got %d", int64(len(data))-100, pos)
	}

	// Seek before start should error
	_, err = ext4File.Seek(-1, io.SeekStart)
	if err == nil {
		t.Errorf("expected error seeking before start of file")
	}
}

// TestReadAtEOF tests reading at exactly the end of a file
func TestReadAtEOF(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	data := []byte("exactly this much")
	ext4File, err := fs.OpenFile("/eoftest.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if _, err := ext4File.Write(data); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Seek to exact end
	if _, err := ext4File.Seek(int64(len(data)), io.SeekStart); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	buf := make([]byte, 10)
	n, err := ext4File.Read(buf)
	if n != 0 {
		t.Errorf("expected 0 bytes at EOF, got %d", n)
	}
	if err != io.EOF {
		t.Errorf("expected io.EOF at end of file, got %v", err)
	}
}

// TestWriteOnExistingImage tests writing on an image that was Read() from disk,
// verifying that writes to an existing filesystem work correctly and that the
// resulting image passes e2fsck validation.
func TestWriteOnExistingImage(t *testing.T) {
	_ = testCreateImgCopyFrom(t, imgFile) // ensure test image is available
	outfile := testCreateImgCopyFrom(t, imgFile)
	f, err := os.OpenFile(outfile, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("Error opening test image: %v", err)
	}
	defer f.Close()

	b := file.New(f, false)
	fs, err := Read(b, 100*MB, 0, 512)
	if err != nil {
		t.Fatalf("Read filesystem failed: %v", err)
	}

	// Write a multi-block file to the existing filesystem
	data := make([]byte, 16384) // 4 blocks
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read failed: %v", err)
	}

	ext4File, err := fs.OpenFile("/newmultiblock.dat", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	n, err := ext4File.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("short write: expected %d, got %d", len(data), n)
	}

	// Read back
	if _, err := ext4File.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	readBuf := make([]byte, len(data))
	nRead, err := ext4File.Read(readBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: %v", err)
	}
	if nRead != len(data) {
		t.Fatalf("short read: expected %d, got %d", len(data), nRead)
	}
	if !bytes.Equal(data, readBuf) {
		t.Errorf("data mismatch on existing image write")
	}

	// Validate with e2fsck
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	f.Close()
	cmd := exec.Command("e2fsck", "-f", "-n", outfile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("e2fsck failed: %v\n%s", err, string(out))
	}
}

// createFragmentedFile writes a file whose data ends up in two separate
// extents: the first chunk allocates extent A, pad files grab the blocks
// behind it, and the append allocates extent B elsewhere on disk.
// With the default 1 KiB block size it produces:
//
//	extent A: fileBlock 0, count 2  (covers file offsets [0, 2048))
//	pad files in between
//	extent B: fileBlock 2, count 8  (covers file offsets [2048, 10240))
//
// It returns the file content expected when reading the whole file.
func createFragmentedFile(t *testing.T, fs *FileSystem, name string, fillA, fillB byte) []byte {
	t.Helper()
	blockSize := int(fs.superblock.blockSize)
	aBlocks, bBlocks := 2, 8

	write := func(p string, data []byte) {
		f, err := fs.OpenFile(p, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatalf("OpenFile %s: %v", p, err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatalf("Write %s: %v", p, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close %s: %v", p, err)
		}
	}

	write(name, bytes.Repeat([]byte{fillA}, aBlocks*blockSize))
	for i := 0; i < 3; i++ {
		write(fmt.Sprintf("/pad-%s-%d.dat", path.Base(name), i), bytes.Repeat([]byte{'P'}, blockSize))
	}
	f, err := fs.OpenFile(name, os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile %s for append: %v", name, err)
	}
	if _, err := f.Seek(int64(aBlocks*blockSize), io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{fillB}, bBlocks*blockSize)); err != nil {
		t.Fatalf("Write append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// sanity: the file really is fragmented, otherwise the test proves nothing
	r, err := fs.OpenFile(name, os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile %s for inspect: %v", name, err)
	}
	filePtr, ok := r.(*File)
	if !ok {
		t.Fatalf("expected *File, got %T", r)
	}
	exts := filePtr.extents
	if len(exts) < 2 {
		t.Fatalf("expected at least 2 extents, got %d: %+v", len(exts), exts)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	content := append(bytes.Repeat([]byte{fillA}, aBlocks*blockSize), bytes.Repeat([]byte{fillB}, bBlocks*blockSize)...)
	return content
}

// TestReadUnalignedOffsetFragmented reads a fragmented file at offsets that
// are NOT multiples of the block size. An extent covers file blocks
// [fileBlock, fileBlock+count); with the old strict-< skip condition, an
// extent ending exactly at the read start block was not skipped, which made
// leftInExtent negative and made make([]byte, n) panic with
// "makeslice: len out of range" whenever the offset fell inside the block
// right after an extent's end. Regression test for that panic.
func TestReadUnalignedOffsetFragmented(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	blockSize := int(fs.superblock.blockSize)
	content := createFragmentedFile(t, fs, "/frag.dat", 'F', 'G')

	// extent A ends at offset 2*blockSize. The vulnerable window is
	// (2*blockSize, 3*blockSize); a read starting anywhere inside it with a
	// non-aligned offset used to panic.
	tests := []struct {
		name   string
		offset int64
	}{
		{"extent boundary aligned", int64(2 * blockSize)},
		{"one past boundary", int64(2*blockSize + 1)},
		{"mid vulnerable block", int64(2*blockSize + blockSize/2)},
		{"last byte of vulnerable block", int64(3*blockSize - 1)},
		{"unaligned inside second extent", int64(4*blockSize + 123)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := fs.OpenFile("/frag.dat", os.O_RDONLY)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer r.Close()
			if _, err := r.Seek(tt.offset, io.SeekStart); err != nil {
				t.Fatalf("Seek: %v", err)
			}
			buf := make([]byte, 100)
			n, err := r.Read(buf)
			if err != nil && err != io.EOF {
				t.Fatalf("Read at %d: %v", tt.offset, err)
			}
			if n != len(buf) {
				t.Fatalf("short read at %d: got %d, want %d", tt.offset, n, len(buf))
			}
			if !bytes.Equal(buf, content[tt.offset:tt.offset+int64(len(buf))]) {
				t.Errorf("data mismatch at offset %d", tt.offset)
			}
		})
	}
}

// TestWriteUnalignedOffsetFragmented is the Write-side twin of
// TestReadUnalignedOffsetFragmented: writing at an unaligned offset inside
// the block right after an extent's end used to panic the same way.
func TestWriteUnalignedOffsetFragmented(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	blockSize := int(fs.superblock.blockSize)
	createFragmentedFile(t, fs, "/frag.dat", 'F', 'G')

	offsets := []int64{
		int64(2*blockSize + 1),
		int64(2*blockSize + blockSize/2),
		int64(3*blockSize - 1),
		int64(4*blockSize + 123),
	}
	for _, off := range offsets {
		w, err := fs.OpenFile("/frag.dat", os.O_RDWR)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		if _, err := w.Seek(off, io.SeekStart); err != nil {
			t.Fatalf("Seek: %v", err)
		}
		patch := bytes.Repeat([]byte{'X'}, 50)
		if _, err := w.Write(patch); err != nil {
			t.Fatalf("Write at %d: %v", off, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close w: %v", err)
		}

		r, err := fs.OpenFile("/frag.dat", os.O_RDONLY)
		if err != nil {
			t.Fatalf("OpenFile for verify: %v", err)
		}
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			t.Fatalf("Seek verify: %v", err)
		}
		got := make([]byte, len(patch))
		if _, err := r.Read(got); err != nil && err != io.EOF {
			t.Fatalf("Read verify: %v", err)
		}
		if !bytes.Equal(got, patch) {
			t.Errorf("write at %d not read back correctly", off)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("Close r: %v", err)
		}
	}
}

// TestCopyFileUnalignedChunkFragmented simulates a copy-based rename: a
// fragmented file larger than one block is copied with a chunk size that is
// not a multiple of the block size, so read offsets become unaligned. With
// the old skip condition the second read panicked.
func TestCopyFileUnalignedChunkFragmented(t *testing.T) {
	_, f := testCreateEmptyFile(t, 100*MB)
	defer f.Close()

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	content := createFragmentedFile(t, fs, "/src.dat", 'S', 'T')

	dst, err := fs.OpenFile("/src.dat.new", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile dst: %v", err)
	}
	defer dst.Close()
	src, err := fs.OpenFile("/src.dat", os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile src: %v", err)
	}
	defer src.Close()

	// 3000 is not a multiple of the 1024 block size, so the second read
	// starts at offset 3000, inside the block right after extent A's end.
	buf := make([]byte, 3000)
	var copied int64
	for copied < int64(len(content)) {
		n, err := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				t.Fatalf("Write dst: %v", err)
			}
			copied += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read src: %v", err)
		}
	}
	if copied != int64(len(content)) {
		t.Fatalf("copied %d bytes, want %d", copied, len(content))
	}

	// verify the copy
	r, err := fs.OpenFile("/src.dat.new", os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile copy for verify: %v", err)
	}
	defer r.Close()
	got := make([]byte, len(content))
	if _, err := r.Read(got); err != nil && err != io.EOF {
		t.Fatalf("Read copy: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("copied data mismatch")
	}
}
