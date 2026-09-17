package ext4

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
)

func TestInodeDeviceNumber(t *testing.T) {
	tests := []struct {
		name          string
		fileType      fileType
		blockPointers [15]uint32
		wantMajor     uint32
		wantMinor     uint32
	}{
		{
			name:      "non-device returns zero",
			fileType:  fileTypeRegularFile,
			wantMajor: 0,
			wantMinor: 0,
		},
		{
			name:     "old-style char device",
			fileType: fileTypeCharacterDevice,
			// major=1, minor=3
			blockPointers: [15]uint32{(1 << 8) | 3},
			wantMajor:     1,
			wantMinor:     3,
		},
		{
			name:     "old-style block device",
			fileType: fileTypeBlockDevice,
			// major=8, minor=0
			blockPointers: [15]uint32{(8 << 8) | 0}, //nolint:staticcheck // (minor=0)
			wantMajor:     8,
			wantMinor:     0,
		},
		{
			name:     "new-style char device with large minor",
			fileType: fileTypeCharacterDevice,
			// major=136, minor=0
			blockPointers: [15]uint32{0, (0 & 0xff) | (136 << 8) | ((0 & ^uint32(0xff)) << 12)},
			wantMajor:     136,
			wantMinor:     0,
		},
		{
			name:     "new-style block device with large minor",
			fileType: fileTypeBlockDevice,
			// major=259, minor=1
			blockPointers: [15]uint32{0, (1 & 0xff) | (259 << 8) | ((1 & ^uint32(0xff)) << 12)},
			wantMajor:     259,
			wantMinor:     1,
		},
		{
			name:     "new-style with large minor number",
			fileType: fileTypeBlockDevice,
			// major=8, minor=256
			blockPointers: [15]uint32{0, (256 & 0xff) | (8 << 8) | ((256 & ^uint32(0xff)) << 12)},
			wantMajor:     8,
			wantMinor:     256,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := &inode{
				fileType:      tt.fileType,
				blockPointers: tt.blockPointers,
			}
			major, minor := in.deviceNumber()
			if major != tt.wantMajor || minor != tt.wantMinor {
				t.Errorf("deviceNumber() = (%d, %d), want (%d, %d)", major, minor, tt.wantMajor, tt.wantMinor)
			}
		})
	}
}

// TestInodeToBytesPreservesExtraSpace checks that rewriting an inode keeps the
// contents of the extra inode space, where ext4 stores small extended
// attributes such as security.capability.
func TestInodeToBytesPreservesExtraSpace(t *testing.T) {
	const (
		inodeNumber = uint32(12)
		extraIsize  = uint16(32)
	)
	sb := &superblock{inodeSize: 256, checksumSeed: 0x12345678}
	xattrStart := int(ext2InodeSize) + int(extraIsize)

	// an inode as it comes off disk: the minimum set of extra fields, a project
	// id, and a single user.foo attribute in the space that follows
	raw := make([]byte, sb.inodeSize)
	binary.LittleEndian.PutUint16(raw[0x0:0x2], 0x8180) // regular file, 0600
	binary.LittleEndian.PutUint16(raw[0x80:0x82], extraIsize)
	binary.LittleEndian.PutUint32(raw[0x98:0x9c], 0x99) // i_version_hi
	binary.LittleEndian.PutUint32(raw[0x9c:0xa0], 0x42) // i_projid
	binary.LittleEndian.PutUint32(raw[xattrStart:xattrStart+4], xattrMagic)
	copy(raw[xattrStart+4:], buildXattrEntry(xattrIndexUser, "foo", []byte("bar")))
	checksum := inodeChecksum(raw, sb.checksumSeed, inodeNumber, 0)
	binary.LittleEndian.PutUint16(raw[0x7c:0x7e], uint16(checksum))
	binary.LittleEndian.PutUint16(raw[0x82:0x84], uint16(checksum>>16))

	expected := bytes.Clone(raw[0x98:])
	in, err := inodeFromBytes(raw, sb, inodeNumber)
	if err != nil {
		t.Fatalf("Error parsing inode: %v", err)
	}
	b := in.toBytes(sb)
	if !bytes.Equal(b[0x98:], expected) {
		t.Errorf("inode.toBytes() changed the extra inode space, actual then expected\n% x\n% x", b[0x98:], expected)
	}
}

// TestRemoveWipesInodeExtendedAttributes checks that removing a file clears the extended
// attributes held inside its inode. The entry is freed for reuse, so bytes left there
// belong to whichever file takes the slot next. No API sets an in-inode attribute, so
// this writes one through the inode itself.
func TestRemoveWipesInodeExtendedAttributes(t *testing.T) {
	outfile, f := testCreateEmptyFile(t, 100*MB)
	t.Cleanup(func() { _ = f.Close() })

	fs, err := Create(file.New(f, false), 100*MB, 0, 512, &Params{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	fl, err := fs.OpenFile("/victim.txt", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if _, err := fl.Write([]byte("hello")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	_, entry, err := fs.getEntryAndParent("/victim.txt")
	if err != nil {
		t.Fatalf("could not look up /victim.txt: %v", err)
	}
	in, err := fs.readInode(entry.inode)
	if err != nil {
		t.Fatalf("could not read inode %d: %v", entry.inode, err)
	}
	// a file created here declares i_extra_isize 96, leaving 32 bytes of inode tail,
	// where an entry table and its terminator alone need 36. Declare the conventional
	// 32 instead, as mke2fs does, which leaves room for the attribute.
	in.inodeSize = minInodeSize + 32
	xattrStart := int(ext2InodeSize) + int(in.inodeSize-minInodeSize)
	blob := make([]byte, int(fs.superblock.inodeSize)-xattrStart)
	xattrEntry := buildXattrEntry(xattrIndexUser, "foo", []byte("bar"))
	if len(blob) < 4+len(xattrEntry) {
		t.Fatalf("the inode tail holds %d bytes, too few for the %d the attribute needs", len(blob), 4+len(xattrEntry))
	}
	binary.LittleEndian.PutUint32(blob[0:4], xattrMagic)
	copy(blob[4:], xattrEntry)
	in.ibodyXattrs = string(blob)
	if err := fs.writeInode(in); err != nil {
		t.Fatalf("could not write inode %d: %v", entry.inode, err)
	}
	if planted, err := fs.readInode(entry.inode); err != nil {
		t.Fatalf("could not read inode %d back: %v", entry.inode, err)
	} else if planted.ibodyXattrs != string(blob) {
		t.Fatalf("the attributes did not reach the inode, so this test checks nothing")
	}
	if attrs, err := fs.GetXattr("/victim.txt"); err != nil {
		t.Fatalf("GetXattr failed: %v", err)
	} else if got := string(attrs["user.foo"]); got != "bar" {
		t.Fatalf("the planted attribute reads back as %q, expected \"bar\"", got)
	}

	if err := fs.Remove("/victim.txt"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	removed, err := fs.readInode(entry.inode)
	if err != nil {
		t.Fatalf("could not read the removed inode %d: %v", entry.inode, err)
	}
	for i := 0; i < len(removed.ibodyXattrs); i++ {
		if removed.ibodyXattrs[i] != 0 {
			t.Fatalf("byte %d of the freed inode is %#x, expected the attributes to be gone", i, removed.ibodyXattrs[i])
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	cmd := exec.Command("e2fsck", "-f", "-n", outfile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("e2fsck failed: %v\n%s", err, string(out))
	}
}

// TestInodeFromBytesShortInode checks that an inode of fewer than 256 bytes parses.
// The project id sits at 0x9c, and reading it from a slice that ran to 0x100 indexed
// past the end of any inode between 160 and 255 bytes.
func TestInodeFromBytesShortInode(t *testing.T) {
	const (
		inodeNumber = uint32(12)
		extraIsize  = uint16(32)
		projectID   = uint32(0x42)
	)
	sb := &superblock{inodeSize: 200, checksumSeed: 0x12345678}

	raw := make([]byte, sb.inodeSize)
	binary.LittleEndian.PutUint16(raw[0x0:0x2], 0x8180) // regular file, 0600
	binary.LittleEndian.PutUint16(raw[0x80:0x82], extraIsize)
	binary.LittleEndian.PutUint32(raw[0x9c:0xa0], projectID)
	checksum := inodeChecksum(raw, sb.checksumSeed, inodeNumber, 0)
	binary.LittleEndian.PutUint16(raw[0x7c:0x7e], uint16(checksum))
	binary.LittleEndian.PutUint16(raw[0x82:0x84], uint16(checksum>>16))

	in, err := inodeFromBytes(raw, sb, inodeNumber)
	if err != nil {
		t.Fatalf("Error parsing inode: %v", err)
	}
	if in.project != projectID {
		t.Errorf("project id is %#x, expected %#x", in.project, projectID)
	}
}
