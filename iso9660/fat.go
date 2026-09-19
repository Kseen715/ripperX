package iso9660

import (
	"errors"
	"io"
)

// A FAT reader lives here because of where the interesting images hide.
//
// Half the installer images in the world put their UEFI bootloaders in the
// ISO 9660 tree, where the rest of this package can read them. The other
// half - every Windows install image, Proxmox, the Void live images - are
// UDF bridge discs: their ISO 9660 tree holds a readme and nothing else,
// and the whole EFI boot path exists only inside the El Torito boot image,
// which is a small FAT filesystem.
//
// So this reads just enough FAT to walk to /EFI/BOOT and look at what is
// there. It is deliberately not a filesystem: no long names, no writing, no
// chain reading past the first cluster of a file. What it answers is "which
// bootloaders does this image carry", and the answer is in the directory
// names and the first few bytes of each file.

var errNotFAT = errors.New("not a FAT filesystem")

type fatFS struct {
	r    io.ReaderAt
	base int64 // where the FAT volume starts inside the containing image

	bytesPerSector  int64
	sectorsPerClust int64
	reserved        int64
	numFATs         int64
	sectorsPerFAT   int64
	rootEntries     int64
	rootCluster     int64 // FAT32 only
	rootDirSectors  int64
	firstDataSector int64
	bits            int // 12, 16 or 32
}

// openFAT reads the BIOS parameter block at base and works out the geometry.
// Everything it rejects, it rejects because the numbers cannot describe a
// filesystem - which is the normal case, since most things are not one.
func openFAT(r io.ReaderAt, base int64) (*fatFS, error) {
	b := make([]byte, 512)
	if _, err := r.ReadAt(b, base); err != nil {
		return nil, err
	}
	// A FAT volume starts with a jump instruction; nothing else here does.
	if b[0] != 0xeb && b[0] != 0xe9 {
		return nil, errNotFAT
	}
	f := &fatFS{
		r:               r,
		base:            base,
		bytesPerSector:  int64(le16(b[11:13])),
		sectorsPerClust: int64(b[13]),
		reserved:        int64(le16(b[14:16])),
		numFATs:         int64(b[16]),
		rootEntries:     int64(le16(b[17:19])),
		sectorsPerFAT:   int64(le16(b[22:24])),
	}
	switch f.bytesPerSector {
	case 512, 1024, 2048, 4096:
	default:
		return nil, errNotFAT
	}
	if f.sectorsPerClust == 0 || f.sectorsPerClust&(f.sectorsPerClust-1) != 0 || f.sectorsPerClust > 128 {
		return nil, errNotFAT
	}
	if f.numFATs == 0 || f.numFATs > 2 || f.reserved == 0 {
		return nil, errNotFAT
	}

	total := int64(le16(b[19:21]))
	if total == 0 {
		total = int64(le32(b[32:36]))
	}
	if f.sectorsPerFAT == 0 { // FAT32 puts it in the extended block
		f.sectorsPerFAT = int64(le32(b[36:40]))
		f.rootCluster = int64(le32(b[44:48]))
	}
	if f.sectorsPerFAT == 0 || total == 0 {
		return nil, errNotFAT
	}

	f.rootDirSectors = (f.rootEntries*32 + f.bytesPerSector - 1) / f.bytesPerSector
	f.firstDataSector = f.reserved + f.numFATs*f.sectorsPerFAT + f.rootDirSectors
	if f.firstDataSector >= total {
		return nil, errNotFAT
	}
	// Which FAT this is is decided by the cluster count and nothing else -
	// not by the string in the boot sector, which lies often enough that the
	// specification says to ignore it.
	clusters := (total - f.firstDataSector) / f.sectorsPerClust
	switch {
	case clusters < 4085:
		f.bits = 12
	case clusters < 65525:
		f.bits = 16
	default:
		f.bits = 32
	}
	if f.bits == 32 && f.rootCluster < 2 {
		return nil, errNotFAT
	}
	return f, nil
}

type fatEntry struct {
	name    string
	cluster int64
	size    int64
	isDir   bool
}

func (f *fatFS) at(sector int64) int64 { return f.base + sector*f.bytesPerSector }

func (f *fatFS) clusterSector(cluster int64) int64 {
	return f.firstDataSector + (cluster-2)*f.sectorsPerClust
}

// next follows the allocation chain. The three FAT widths differ only here.
func (f *fatFS) next(cluster int64) (int64, error) {
	fatStart := f.at(f.reserved)
	switch f.bits {
	case 32:
		b := make([]byte, 4)
		if _, err := f.r.ReadAt(b, fatStart+cluster*4); err != nil {
			return 0, err
		}
		return int64(le32(b) & 0x0fffffff), nil
	case 16:
		b := make([]byte, 2)
		if _, err := f.r.ReadAt(b, fatStart+cluster*2); err != nil {
			return 0, err
		}
		return int64(le16(b)), nil
	default:
		// Twelve bits per entry: two entries share three bytes, and which
		// nibble belongs to which depends on the parity of the cluster.
		b := make([]byte, 2)
		if _, err := f.r.ReadAt(b, fatStart+cluster+cluster/2); err != nil {
			return 0, err
		}
		v := int64(le16(b))
		if cluster&1 == 1 {
			return v >> 4, nil
		}
		return v & 0x0fff, nil
	}
}

func (f *fatFS) endOfChain(cluster int64) bool {
	switch f.bits {
	case 32:
		return cluster >= 0x0ffffff8 || cluster < 2
	case 16:
		return cluster >= 0xfff8 || cluster < 2
	default:
		return cluster >= 0xff8 || cluster < 2
	}
}

// maxDirClusters stops a corrupt or hostile image from spinning here. A
// directory of ten thousand entries is already past anything real.
const maxDirClusters = 512

// readDir lists one directory. cluster 0 means the fixed root area of a
// FAT12 or FAT16 volume, which is not part of the cluster space at all.
func (f *fatFS) readDir(cluster int64) ([]fatEntry, error) {
	var out []fatEntry
	read := func(off, length int64) (bool, error) {
		buf := make([]byte, length)
		if _, err := f.r.ReadAt(buf, off); err != nil {
			return false, err
		}
		for i := int64(0); i+32 <= length; i += 32 {
			rec := buf[i : i+32]
			switch rec[0] {
			case 0x00:
				return true, nil // no entry after this one
			case 0xe5:
				continue // deleted
			}
			attr := rec[11]
			if attr&0x0f == 0x0f || attr&0x08 != 0 {
				continue // a long-name fragment, or the volume label
			}
			out = append(out, fatEntry{
				name:    shortName(rec[:11]),
				cluster: int64(le16(rec[26:28])) | int64(le16(rec[20:22]))<<16,
				size:    int64(le32(rec[28:32])),
				isDir:   attr&0x10 != 0,
			})
		}
		return false, nil
	}

	if cluster == 0 && f.bits != 32 {
		done, err := read(f.at(f.reserved+f.numFATs*f.sectorsPerFAT), f.rootDirSectors*f.bytesPerSector)
		_ = done
		return out, err
	}
	if cluster == 0 {
		cluster = f.rootCluster
	}
	size := f.sectorsPerClust * f.bytesPerSector
	for n := 0; n < maxDirClusters; n++ {
		done, err := read(f.at(f.clusterSector(cluster)), size)
		if err != nil {
			return out, err
		}
		if done {
			return out, nil
		}
		nextCluster, err := f.next(cluster)
		if err != nil {
			return out, err
		}
		if f.endOfChain(nextCluster) {
			return out, nil
		}
		cluster = nextCluster
	}
	return out, nil
}

// find walks a slash-separated path, matching names without regard to case
// because FAT short names are uppercase by rule and nobody writes them that
// way.
func (f *fatFS) find(parts ...string) ([]fatEntry, error) {
	cluster := int64(0)
	for _, part := range parts {
		entries, err := f.readDir(cluster)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range entries {
			if e.isDir && equalFold(e.name, part) {
				cluster, found = e.cluster, true
				break
			}
		}
		if !found {
			return nil, ErrNotFound
		}
	}
	return f.readDir(cluster)
}

// head reads the start of a file, which is all that is wanted: the machine
// type of an executable is in its first few hundred bytes. It reads from
// the first cluster only and so never has to follow a chain.
func (f *fatFS) head(e fatEntry, n int64) []byte {
	if e.cluster < 2 || e.size == 0 {
		return nil
	}
	if max := f.sectorsPerClust * f.bytesPerSector; n > max {
		n = max
	}
	if n > e.size {
		n = e.size
	}
	buf := make([]byte, n)
	if _, err := f.r.ReadAt(buf, f.at(f.clusterSector(e.cluster))); err != nil {
		return nil
	}
	return buf
}

// shortName turns the 8.3 field into a name with a dot in it.
func shortName(b []byte) string {
	base := trimSpace(string(b[:8]))
	ext := trimSpace(string(b[8:11]))
	if ext == "" {
		return base
	}
	return base + "." + ext
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == 0) {
		s = s[:len(s)-1]
	}
	return s
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
