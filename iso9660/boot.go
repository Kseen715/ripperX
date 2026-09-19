package iso9660

import (
	"errors"
	"fmt"
	"io"
	"sort"
)

// Whether a burned disc will boot is decided entirely by what is in the
// image, and it is worth knowing before spending a disc rather than after.
//
// An ISO that boots carries an El Torito boot record: a volume descriptor
// at sector 17 pointing at a boot catalogue, and the catalogue says which
// firmware the image caters for. A disc written from such an image boots,
// because the record and everything it points at are inside the image and
// are written with it.
//
// An image with no such record will produce a perfectly good data disc that
// no machine will boot from - and that is the case worth warning about,
// because a disk image meant for a USB stick is the same length and the
// same shape and fails silently.

// BootPlatform is the firmware a boot entry caters for.
type BootPlatform struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// BootInfo is what an image says about booting.
type BootInfo struct {
	// Bootable is whether there is an El Torito record with at least one
	// entry marked bootable.
	Bootable bool `json:"bootable"`
	// Platforms are the firmwares catered for, in the order the catalogue
	// lists them. A modern installer image usually lists two: the PC BIOS
	// and UEFI.
	Platforms []BootPlatform `json:"platforms,omitempty"`
	// Architectures are the processor architectures the image carries
	// bootloaders for, lowest name first: "x86_64", "aarch64", "i386".
	// They are read out of the bootloaders themselves, so an image that
	// says one thing in its file name and another in its contents is
	// reported by its contents.
	//
	// It can be empty for an image that really does boot: a BIOS-only
	// image has one 16-bit entry point and nothing in it names an
	// architecture, because in 1995 there was only one.
	Architectures []string `json:"architectures,omitempty"`
	// Emulation is what the BIOS is asked to pretend the boot image is.
	// "none" is what everything modern uses.
	Emulation string `json:"emulation,omitempty"`
	// CatalogSector is where the boot catalogue lives, which is the one
	// number worth having if any of this has to be checked by hand.
	CatalogSector int64 `json:"catalogSector,omitempty"`
	// Why says what was found when nothing was, so "not bootable" can be
	// told apart from "could not tell".
	Why string `json:"why,omitempty"`
}

// ErrNoBootRecord means the image is a valid ISO 9660 with no El Torito
// record in it. It is not a failure: most discs are not meant to boot.
var ErrNoBootRecord = errors.New("this image has no boot record")

const (
	// bootRecordSector is where the boot record volume descriptor sits, if
	// there is one: immediately after the primary descriptor.
	bootRecordSector = 17
	elToritoID       = "EL TORITO SPECIFICATION"
)

// bootPlatforms names the firmware ids El Torito defines. Anything else is
// reported by its number rather than hidden, because an image that caters
// for something this table has not heard of is still telling the truth.
var bootPlatformNames = map[int]string{
	0x00: "PC BIOS",
	0x01: "PowerPC",
	0x02: "Macintosh",
	0xef: "UEFI",
}

func platformName(id int) string {
	if n := bootPlatformNames[id]; n != "" {
		return n
	}
	return fmt.Sprintf("platform %#02x", id)
}

var emulationNames = map[byte]string{
	0: "none",
	1: "1.2 MB floppy",
	2: "1.44 MB floppy",
	3: "2.88 MB floppy",
	4: "hard disk",
}

// ReadBootInfo looks for an El Torito boot record and reports what it says.
// r is the image, read at byte offsets, exactly as Open takes it.
//
// An image that is not ISO 9660 at all gives ErrNotISO9660: that matters
// because a disk image meant for a USB stick is also a whole number of
// sectors and passes every other check a burn makes, and writing one to a
// disc produces a coaster that looks like a success.
func ReadBootInfo(r io.ReaderAt) (*BootInfo, error) {
	primary := make([]byte, BlockSize)
	if _, err := r.ReadAt(primary, systemArea*BlockSize); err != nil {
		return nil, fmt.Errorf("reading the volume descriptor: %w", err)
	}
	if string(primary[1:6]) != "CD001" {
		return nil, ErrNotISO9660
	}

	rec := make([]byte, BlockSize)
	if _, err := r.ReadAt(rec, bootRecordSector*BlockSize); err != nil {
		return nil, fmt.Errorf("reading the boot record: %w", err)
	}
	// A boot record is descriptor type 0. Anything else here - usually the
	// terminator - means this image was never meant to boot.
	if rec[0] != 0x00 || string(rec[1:6]) != "CD001" {
		return &BootInfo{Why: "there is no boot record in this image"}, nil
	}
	if id := trimRight(string(rec[7:39])); id != elToritoID {
		return &BootInfo{Why: fmt.Sprintf(
			"this image has a boot record for %q, which is not the El Torito one a PC uses", id)}, nil
	}

	catalog := int64(le32(rec[71:75]))
	info := &BootInfo{CatalogSector: catalog}
	buf := make([]byte, BlockSize)
	if _, err := r.ReadAt(buf, catalog*BlockSize); err != nil {
		info.Why = fmt.Sprintf("the boot catalogue at sector %d could not be read: %v", catalog, err)
		return info, nil
	}

	// The validation entry comes first: it names the first platform and
	// ends with a fixed pair of bytes that says the catalogue is real.
	if len(buf) < 64 || buf[0] != 0x01 || buf[30] != 0x55 || buf[31] != 0xaa {
		info.Why = "the boot catalogue is not in the expected form"
		return info, nil
	}
	first := int(buf[1])
	info.Platforms = append(info.Platforms, BootPlatform{ID: first, Name: platformName(first)})

	// Each entry says where its boot image is, and for a UEFI entry that
	// image is a small FAT filesystem with the bootloaders in it - which on
	// a UDF bridge disc is the only place they exist.
	var images []bootImage

	// The default entry follows, then any number of section headers, each
	// introducing entries for another platform.
	if len(buf) >= 64 && buf[32] == 0x88 {
		info.Bootable = true
		info.Emulation = emulationNames[buf[33]&0x0f]
		if info.Emulation == "" {
			info.Emulation = fmt.Sprintf("media type %d", buf[33]&0x0f)
		}
		images = append(images, bootImage{platform: first, sector: int64(le32(buf[40:44]))})
	}
	for off := 64; off+32 <= len(buf); {
		header := buf[off]
		if header != 0x90 && header != 0x91 {
			break
		}
		id := int(buf[off+1])
		if !hasPlatform(info.Platforms, id) {
			info.Platforms = append(info.Platforms, BootPlatform{ID: id, Name: platformName(id)})
		}
		entries := int(buf[off+2]) | int(buf[off+3])<<8
		off += 32
		for i := 0; i < entries && off+32 <= len(buf); i++ {
			if buf[off] == 0x88 {
				info.Bootable = true
				images = append(images, bootImage{platform: id, sector: int64(le32(buf[off+8 : off+12]))})
			}
			off += 32
		}
		if header == 0x91 { // the last header
			break
		}
	}

	if !info.Bootable && info.Why == "" {
		info.Why = "this image has a boot catalogue, but no entry in it is marked bootable"
		return info, nil
	}
	info.Architectures = architectures(r, images)
	return info, nil
}

// bootImage is one entry's boot image: which firmware it is for, and where
// in the volume it starts.
type bootImage struct {
	platform int
	sector   int64
}

const platformUEFI = 0xef

// architectures works out what the image can actually boot, by looking at
// the bootloaders rather than at the name of the file.
//
// There are two places to look, and both are needed. Most Linux installers
// put /EFI/BOOT in the ISO 9660 tree, where it can be read directly. Every
// Windows install image, and a few others, are UDF bridge discs whose ISO
// 9660 tree holds a readme and nothing else - for those the only copy of
// the bootloaders is inside the El Torito boot image, which is FAT.
func architectures(r io.ReaderAt, images []bootImage) []string {
	found := map[string]bool{}

	if fs, err := Open(r); err == nil {
		if dir, err := fs.ReadDir("/EFI/BOOT"); err == nil {
			for _, e := range dir {
				if e.IsDir {
					continue
				}
				if a := archOfEFI(e.Name, readHead(fs, e)); a != "" {
					found[a] = true
				}
			}
		}
	}

	if len(found) == 0 {
		for _, img := range images {
			if img.platform != platformUEFI || img.sector <= 0 {
				continue
			}
			fat, err := openFAT(r, img.sector*BlockSize)
			if err != nil {
				continue
			}
			dir, err := fat.find("EFI", "BOOT")
			if err != nil {
				continue
			}
			for _, e := range dir {
				if e.isDir {
					continue
				}
				if a := archOfEFI(e.name, fat.head(e, 1024)); a != "" {
					found[a] = true
				}
			}
		}
	}

	if len(found) == 0 {
		return nil
	}
	out := make([]string, 0, len(found))
	for a := range found {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func readHead(fs *FS, e Entry) []byte {
	r, _, err := fs.Open(e.Path)
	if err != nil {
		return nil
	}
	n := int64(1024)
	if e.Size < n {
		n = e.Size
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, 0); err != nil {
		return nil
	}
	return buf
}

// efiNames is the UEFI removable-media boot path: the firmware looks for
// exactly this name, so the name is not a convention someone chose, it is
// the architecture written down.
var efiNames = map[string]string{
	"bootx64.efi":         "x86_64",
	"bootia32.efi":        "i386",
	"bootaa64.efi":        "aarch64",
	"bootarm.efi":         "arm",
	"bootia64.efi":        "ia64",
	"bootriscv32.efi":     "riscv32",
	"bootriscv64.efi":     "riscv64",
	"bootloongarch64.efi": "loongarch64",
}

// peMachines are the machine types a UEFI executable can carry. This is the
// stronger answer of the two, because it comes out of the binary.
var peMachines = map[uint16]string{
	0x014c: "i386",
	0x8664: "x86_64",
	0xaa64: "aarch64",
	0x01c0: "arm",
	0x01c2: "arm",
	0x01c4: "arm",
	0x0200: "ia64",
	0x5032: "riscv32",
	0x5064: "riscv64",
	0x6264: "loongarch64",
}

// archOfEFI reads the architecture of one file in /EFI/BOOT. Only the
// firmware's own boot names are considered: grubx64.efi sitting beside
// bootx64.efi says nothing new, and a stray file with a .efi suffix should
// not add an architecture nothing will ever boot.
func archOfEFI(name string, head []byte) string {
	byName, ok := efiNames[lower(name)]
	if !ok {
		return ""
	}
	if m := peMachine(head); m != "" {
		return m
	}
	return byName
}

// peMachine pulls the machine type out of a PE executable, which is what a
// UEFI bootloader is.
func peMachine(head []byte) string {
	if len(head) < 0x40 || head[0] != 'M' || head[1] != 'Z' {
		return ""
	}
	off := int64(le32(head[0x3c:0x40]))
	if off < 0 || off+6 > int64(len(head)) {
		return ""
	}
	if string(head[off:off+4]) != "PE\x00\x00" {
		return ""
	}
	return peMachines[le16(head[off+4:off+6])]
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func hasPlatform(list []BootPlatform, id int) bool {
	for _, p := range list {
		if p.ID == id {
			return true
		}
	}
	return false
}

func trimRight(s string) string {
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == 0) {
		s = s[:len(s)-1]
	}
	return s
}

// Summary is the one sentence to put in front of someone about to spend a
// disc on this image.
func (b *BootInfo) Summary() string {
	if b == nil {
		return ""
	}
	if !b.Bootable {
		if b.Why != "" {
			return b.Why + " - it will burn to a perfectly good data disc, but nothing will boot from it"
		}
		return "this image will not boot"
	}
	names := make([]string, 0, len(b.Platforms))
	for _, p := range b.Platforms {
		names = append(names, p.Name)
	}
	var s string
	switch len(names) {
	case 0:
		s = "this image is bootable"
	default:
		s = "this image boots on " + joinAnd(names)
	}
	if len(b.Architectures) > 0 {
		s += ", on " + joinAnd(b.Architectures)
	}
	return s
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	out := ""
	for i, s := range items[:len(items)-1] {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out + " and " + items[len(items)-1]
}
