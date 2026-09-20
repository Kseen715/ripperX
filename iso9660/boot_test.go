package iso9660

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The layouts these tests build are the two real ones, and they are
// genuinely different shapes.
//
// A Linux installer keeps its bootloaders in the ISO 9660 tree, at
// /EFI/BOOT, where they can simply be read. A Windows install image is a
// UDF bridge disc: its ISO 9660 tree holds a readme, and the only copy of
// the bootloaders is inside the El Torito boot image, which is a FAT
// filesystem. Both have to work, because the second is what half a shelf of
// installer images actually is.

const (
	bootPVD  = 16
	bootRec  = 17
	bootTerm = 18
	bootRoot = 19
	bootCat  = 20
	bootEFI  = 21 // the EFI directory
	bootDir  = 22 // EFI/BOOT
	bootFile = 23 // the bootloader itself
	bootFAT  = 24 // where a FAT boot image starts, when there is one
	bootSize = 64 // sectors in the whole test volume
)

type bootOpts struct {
	// tree puts a bootloader of this name in the ISO 9660 tree at
	// /EFI/BOOT, the way a Linux installer does.
	tree string
	// fat puts one of this name inside a FAT El Torito boot image, the way
	// a Windows install image does.
	fat     string
	machine uint16
	// unbootable writes a catalogue whose entries are all marked not
	// bootable.
	unbootable bool
}

// peHeader is the smallest thing that answers "what does this run on":
// the DOS stub's pointer, the PE signature, and the machine type.
func peHeader(machine uint16) []byte {
	b := make([]byte, 0x100)
	b[0], b[1] = 'M', 'Z'
	putLE32(b[0x3c:0x40], 0x80)
	copy(b[0x80:], "PE\x00\x00")
	b[0x84], b[0x85] = byte(machine), byte(machine>>8)
	return b
}

func buildBootISO(t *testing.T, o bootOpts) []byte {
	t.Helper()
	img := make([]byte, bootSize*BlockSize)
	sector := func(n int) []byte { return img[n*BlockSize : (n+1)*BlockSize] }

	pvd := sector(bootPVD)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[8:40], pad("BOOTTEST", 32))
	copy(pvd[40:72], pad("BOOT TEST", 32))
	both32(pvd[80:88], bootSize)
	copy(pvd[156:190], dirRecord("\x00", bootRoot, BlockSize, true, nil))

	// The boot record: a volume descriptor of type 0 that names El Torito
	// and says where the catalogue is.
	rec := sector(bootRec)
	rec[0] = 0
	copy(rec[1:6], "CD001")
	rec[6] = 1
	copy(rec[7:39], pad(elToritoID, 32))
	putLE32(rec[71:75], bootCat)

	term := sector(bootTerm)
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1

	root := sector(bootRoot)
	off := 0
	put := func(dst, r []byte) { copy(dst[off:], r); off += len(r) }
	put(root, dirRecord("\x00", bootRoot, BlockSize, true, nil))
	put(root, dirRecord("\x01", bootRoot, BlockSize, true, nil))
	if o.tree != "" {
		put(root, dirRecord("EFI", bootEFI, BlockSize, true, nil))

		pe := peHeader(o.machine)
		efi := sector(bootEFI)
		off = 0
		put(efi, dirRecord("\x00", bootEFI, BlockSize, true, nil))
		put(efi, dirRecord("\x01", bootRoot, BlockSize, true, nil))
		put(efi, dirRecord("BOOT", bootDir, BlockSize, true, nil))

		dir := sector(bootDir)
		off = 0
		put(dir, dirRecord("\x00", bootDir, BlockSize, true, nil))
		put(dir, dirRecord("\x01", bootEFI, BlockSize, true, nil))
		put(dir, dirRecord(o.tree+";1", bootFile, uint32(len(pe)), false, nil))
		copy(sector(bootFile), pe)
	}

	if o.fat != "" {
		copy(img[bootFAT*BlockSize:], fatImage(t, o.fat, peHeader(o.machine)))
	}

	cat := sector(bootCat)
	cat[0] = 0x01 // validation entry
	cat[1] = 0x00 // for the PC BIOS
	cat[30], cat[31] = 0x55, 0xaa

	flag := byte(0x88)
	if o.unbootable {
		flag = 0x00
	}
	cat[32] = flag // the default entry, which is the BIOS one
	cat[33] = 0    // no emulation
	putLE32(cat[40:44], bootFAT)

	// One section, for UEFI, with one entry in it.
	cat[64] = 0x91 // the final section header
	cat[65] = 0xef // UEFI
	cat[66] = 1    // one entry
	cat[96] = flag
	putLE32(cat[104:108], bootFAT)
	return img
}

// fatImage builds a FAT12 volume with one file at /EFI/BOOT, which is the
// shape of the El Torito boot image on every UEFI install disc.
//
//	0      boot sector
//	1      the FAT
//	2      the root directory
//	3      cluster 2: EFI
//	4      cluster 3: EFI/BOOT
//	5      cluster 4: the file's contents
func fatImage(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	const sectorSize, sectors = 512, 64
	img := make([]byte, sectors*sectorSize)
	at := func(n int) []byte { return img[n*sectorSize : (n+1)*sectorSize] }

	b := at(0)
	b[0], b[1], b[2] = 0xeb, 0x3c, 0x90
	b[11], b[12] = 0x00, 0x02 // 512 bytes per sector
	b[13] = 1                 // one sector per cluster
	b[14], b[15] = 1, 0       // one reserved sector
	b[16] = 1                 // one FAT
	b[17], b[18] = 16, 0      // sixteen root entries, so one sector of them
	b[19], b[20] = sectors, 0
	b[22], b[23] = 1, 0 // one sector per FAT

	// Every FAT entry reads as the end of a chain, which is true: each
	// directory and the file are one cluster long.
	for i := range at(1) {
		at(1)[i] = 0xff
	}

	copy(at(2), fatDirent("EFI        ", 2, 0, true))
	copy(at(3), fatDirent("BOOT       ", 3, 0, true))
	copy(at(4), fatDirent(fatShort(t, name), 4, len(content), false))
	copy(at(5), content)
	return img
}

// fatShort turns a name into the 11-byte 8.3 field, which is how a FAT
// directory stores it.
func fatShort(t *testing.T, name string) string {
	t.Helper()
	base, ext, found := strings.Cut(name, ".")
	if !found || len(base) > 8 || len(ext) > 3 {
		t.Fatalf("%q is not an 8.3 name", name)
	}
	return string(pad(base, 8)) + string(pad(ext, 3))
}

func fatDirent(name11 string, cluster, size int, dir bool) []byte {
	e := make([]byte, 32)
	copy(e[:11], name11)
	if dir {
		e[11] = 0x10
	}
	e[26], e[27] = byte(cluster), byte(cluster>>8)
	putLE32(e[28:32], uint32(size))
	return e
}

func TestBootRecordIsAbsent(t *testing.T) {
	// The plain test volume has a terminator where a boot record would be.
	info, err := ReadBootInfo(bytes.NewReader(buildISO(t)))
	if err != nil {
		t.Fatalf("reading boot info: %v", err)
	}
	if info.Bootable {
		t.Error("a volume with no boot record was reported bootable")
	}
	if info.Why == "" {
		t.Error("nothing said why it will not boot")
	}
	if got := info.Summary(); !bytes.Contains([]byte(got), []byte("data disc")) {
		t.Errorf("summary = %q, wanted it to say what the disc would still be good for", got)
	}
}

func TestNotAnISOIsRefusedRatherThanGuessedAt(t *testing.T) {
	// A disk image meant for a USB stick: the right length, the right
	// shape, and nothing a disc can boot from.
	img := make([]byte, 40*BlockSize)
	copy(img, "\xebXnotanisofilesystematall")
	_, err := ReadBootInfo(bytes.NewReader(img))
	if !errors.Is(err, ErrNotISO9660) {
		t.Fatalf("err = %v, want ErrNotISO9660", err)
	}
}

func TestArchitectureFromTheISOTree(t *testing.T) {
	info, err := ReadBootInfo(bytes.NewReader(
		buildBootISO(t, bootOpts{tree: "BOOTX64.EFI", machine: 0x8664})))
	if err != nil {
		t.Fatalf("reading boot info: %v", err)
	}
	if !info.Bootable {
		t.Fatal("a bootable volume was reported unbootable")
	}
	if len(info.Platforms) != 2 || info.Platforms[0].Name != "PC BIOS" || info.Platforms[1].Name != "UEFI" {
		t.Errorf("platforms = %+v, want PC BIOS then UEFI", info.Platforms)
	}
	if got := info.Architectures; len(got) != 1 || got[0] != "x86_64" {
		t.Errorf("architectures = %v, want [x86_64]", got)
	}
}

// The FAT path is the one that matters for Windows: its ISO 9660 tree has
// no EFI directory at all, so this is the only place the answer exists.
func TestArchitectureFromTheElToritoBootImage(t *testing.T) {
	info, err := ReadBootInfo(bytes.NewReader(
		buildBootISO(t, bootOpts{fat: "BOOTAA64.EFI", machine: 0xaa64})))
	if err != nil {
		t.Fatalf("reading boot info: %v", err)
	}
	if got := info.Architectures; len(got) != 1 || got[0] != "aarch64" {
		t.Errorf("architectures = %v, want [aarch64]", got)
	}
}

// The name is the UEFI boot path and the machine type is the binary. When
// they disagree the binary wins, because that is what the firmware will
// actually try to run.
func TestTheBinaryOutranksItsName(t *testing.T) {
	info, err := ReadBootInfo(bytes.NewReader(
		buildBootISO(t, bootOpts{tree: "BOOTX64.EFI", machine: 0xaa64})))
	if err != nil {
		t.Fatalf("reading boot info: %v", err)
	}
	if got := info.Architectures; len(got) != 1 || got[0] != "aarch64" {
		t.Errorf("architectures = %v, want [aarch64] from the PE header", got)
	}
}

// grubx64.efi sits beside bootx64.efi on most discs and boots nothing by
// itself; only the firmware's own boot path counts.
func TestOnlyTheFirmwareBootPathCounts(t *testing.T) {
	info, err := ReadBootInfo(bytes.NewReader(
		buildBootISO(t, bootOpts{tree: "GRUBX64.EFI", machine: 0x8664})))
	if err != nil {
		t.Fatalf("reading boot info: %v", err)
	}
	if len(info.Architectures) != 0 {
		t.Errorf("architectures = %v, want none", info.Architectures)
	}
	if !info.Bootable {
		t.Error("the image is still bootable; only its architecture is unknown")
	}
}

func TestACatalogueWithNothingBootableInIt(t *testing.T) {
	info, err := ReadBootInfo(bytes.NewReader(
		buildBootISO(t, bootOpts{tree: "BOOTX64.EFI", machine: 0x8664, unbootable: true})))
	if err != nil {
		t.Fatalf("reading boot info: %v", err)
	}
	if info.Bootable {
		t.Error("no entry was marked bootable, but the image was reported bootable")
	}
	if info.Why == "" {
		t.Error("nothing said why")
	}
}

func TestSummaryNamesFirmwareAndArchitecture(t *testing.T) {
	info, err := ReadBootInfo(bytes.NewReader(
		buildBootISO(t, bootOpts{tree: "BOOTX64.EFI", machine: 0x8664})))
	if err != nil {
		t.Fatalf("reading boot info: %v", err)
	}
	want := "this image boots on PC BIOS and UEFI, on x86_64"
	if got := info.Summary(); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestFATRejectsWhatIsNotOne(t *testing.T) {
	// Random bytes, and a volume whose geometry cannot describe a
	// filesystem: neither should be read as one.
	if _, err := openFAT(bytes.NewReader(make([]byte, 4096)), 0); err == nil {
		t.Error("a run of zeroes was accepted as FAT")
	}
	img := fatImage(t, "BOOTX64.EFI", nil)
	img[13] = 3 // sectors per cluster must be a power of two
	if _, err := openFAT(bytes.NewReader(img), 0); err == nil {
		t.Error("a volume with 3 sectors per cluster was accepted")
	}
}
