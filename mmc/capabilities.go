package mmc

import (
	"fmt"
	"sort"
	"strings"
)

// What a drive can do is asked twice, because neither question alone is
// complete. GET CONFIGURATION is the modern one and is authoritative about
// which disc kinds this drive reads and writes. The CD/DVD Capabilities
// mode page is older and half-deprecated, but it is the only place that
// says how big the buffer is, what the loading mechanism looks like, and
// whether the drive can hand over digital audio - and a drive too old to
// answer GET CONFIGURATION answers this one.
//
// Both are optional in their own way, so Capabilities reports what it
// learned and does not fail because one of the two was refused.

// Capabilities is the whole answer to "what is this drive". Can and Cannot
// are the same body of knowledge phrased for a person: every capability the
// probe tested appears in exactly one of them, so the UI can show what a
// drive refuses as plainly as what it offers.
type Capabilities struct {
	Info Info `json:"info"`

	// CurrentProfile is the kind of disc in the drive right now, and
	// Profiles every kind this drive has support for.
	CurrentProfile     Profile   `json:"currentProfile"`
	CurrentProfileName string    `json:"currentProfileName"`
	Profiles           []Profile `json:"profiles"`

	Read  MediaSupport `json:"read"`
	Write MediaSupport `json:"write"`

	// Features of the drive itself rather than of a disc kind.
	CanReadCDText       bool   `json:"canReadCDText"`
	CanReadRawCD        bool   `json:"canReadRawCD"`
	CanReadC2Errors     bool   `json:"canReadC2Errors"`
	CanReadISRC         bool   `json:"canReadIsrc"`
	CanReadUPC          bool   `json:"canReadUpc"`
	CanReadSubchannel   bool   `json:"canReadSubchannel"`
	CanReadAccurateCDDA bool   `json:"canReadAccurateCdda"`
	CanReadMode2Form2   bool   `json:"canReadMode2Form2"`
	CanPlayAudio        bool   `json:"canPlayAudio"`
	CanEject            bool   `json:"canEject"`
	CanLock             bool   `json:"canLock"`
	CanWriteTAO         bool   `json:"canWriteTao"`
	CanWriteSAO         bool   `json:"canWriteSao"`
	CanWriteRaw         bool   `json:"canWriteRaw"`
	CanTestWrite        bool   `json:"canTestWrite"`
	HasBurnProof        bool   `json:"hasBurnProof"`
	CanSetReadSpeed     bool   `json:"canSetReadSpeed"`
	IsMultiRead         bool   `json:"isMultiRead"`
	LoadingMechanism    string `json:"loadingMechanism"`
	BufferKB            int    `json:"bufferKb"`
	MaxReadSpeedKB      int    `json:"maxReadSpeedKb"`
	CurrentReadSpeedKB  int    `json:"currentReadSpeedKb"`
	MaxWriteSpeedKB     int    `json:"maxWriteSpeedKb"`
	WriteSpeedsKB       []int  `json:"writeSpeedsKb"`
	SerialNumber        string `json:"serialNumber,omitempty"`

	// Can and Cannot are the above rendered as sentences, in a fixed order,
	// for a page that wants to show a drive's abilities without knowing what
	// any individual flag means.
	Can    []Ability `json:"can"`
	Cannot []Ability `json:"cannot"`

	// Notes records what could not be asked, so a sparse answer from an old
	// drive does not read as a drive that cannot do anything.
	Notes []string `json:"notes,omitempty"`
}

// Ability is one line of those two lists: a name that does not change, and
// the sentence this package would put on a screen in English.
//
// The name is there because the sentence is not the only rendering there
// will ever be. A page in another language needs something stable to look a
// translation up by, and matching on the English text would mean a
// translation quietly disappearing the day a word in it was improved. The
// name says which ability; which list it is in says whether the drive has
// it.
type Ability struct {
	Name string `json:"name" doc:"a stable name for this ability, to look a translation up by"`
	Text string `json:"text" doc:"the same thing as a sentence, in English"`
}

// MediaSupport is which disc families the drive handles, for reading or for
// writing. Derived from the profile list, which is the drive's own
// statement rather than a guess from its model name.
type MediaSupport struct {
	CD        bool `json:"cd"`
	CDR       bool `json:"cdr"`
	CDRW      bool `json:"cdrw"`
	DVD       bool `json:"dvd"`
	DVDR      bool `json:"dvdr"`
	DVDRW     bool `json:"dvdrw"`
	DVDRAM    bool `json:"dvdram"`
	DVDPlusR  bool `json:"dvdPlusR"`
	DVDPlusRW bool `json:"dvdPlusRw"`
	DualLayer bool `json:"dualLayer"`
	BD        bool `json:"bd"`
	BDR       bool `json:"bdr"`
	BDRE      bool `json:"bdre"`
	HDDVD     bool `json:"hddvd"`
}

// Profile is an MMC profile number: the kind of disc, as the drive names it.
type Profile uint16

const (
	ProfileNone        Profile = 0x0000
	ProfileRemovable   Profile = 0x0002 // a rewritable disk with no defect management
	ProfileCDROM       Profile = 0x0008
	ProfileCDR         Profile = 0x0009
	ProfileCDRW        Profile = 0x000a
	ProfileDVDROM      Profile = 0x0010
	ProfileDVDRSeq     Profile = 0x0011
	ProfileDVDRAM      Profile = 0x0012
	ProfileDVDRWRO     Profile = 0x0013
	ProfileDVDRWSeq    Profile = 0x0014
	ProfileDVDRDLSeq   Profile = 0x0015
	ProfileDVDRDLJump  Profile = 0x0016
	ProfileDVDRWDL     Profile = 0x0017
	ProfileDVDPlusRW   Profile = 0x001a
	ProfileDVDPlusR    Profile = 0x001b
	ProfileDVDPlusRWDL Profile = 0x002a
	ProfileDVDPlusRDL  Profile = 0x002b
	ProfileBDROM       Profile = 0x0040
	ProfileBDRSeq      Profile = 0x0041
	ProfileBDRRandom   Profile = 0x0042
	ProfileBDRE        Profile = 0x0043
	ProfileHDDVDROM    Profile = 0x0050
	ProfileHDDVDR      Profile = 0x0051
	ProfileHDDVDRAM    Profile = 0x0052
)

var profileNames = map[Profile]string{
	ProfileNone:        "no disc",
	ProfileRemovable:   "removable disk",
	ProfileCDROM:       "CD-ROM",
	ProfileCDR:         "CD-R",
	ProfileCDRW:        "CD-RW",
	ProfileDVDROM:      "DVD-ROM",
	ProfileDVDRSeq:     "DVD-R",
	ProfileDVDRAM:      "DVD-RAM",
	ProfileDVDRWRO:     "DVD-RW (read only)",
	ProfileDVDRWSeq:    "DVD-RW",
	ProfileDVDRDLSeq:   "DVD-R DL",
	ProfileDVDRDLJump:  "DVD-R DL (layer jump)",
	ProfileDVDRWDL:     "DVD-RW DL",
	ProfileDVDPlusRW:   "DVD+RW",
	ProfileDVDPlusR:    "DVD+R",
	ProfileDVDPlusRWDL: "DVD+RW DL",
	ProfileDVDPlusRDL:  "DVD+R DL",
	ProfileBDROM:       "BD-ROM",
	ProfileBDRSeq:      "BD-R",
	ProfileBDRRandom:   "BD-R (random)",
	ProfileBDRE:        "BD-RE",
	ProfileHDDVDROM:    "HD DVD-ROM",
	ProfileHDDVDR:      "HD DVD-R",
	ProfileHDDVDRAM:    "HD DVD-RAM",
}

// Name is what to call this profile in the UI. An unknown number is shown
// as a number rather than hidden: a drive that reports something this table
// has never heard of is still telling the truth.
func (p Profile) Name() string {
	if n := profileNames[p]; n != "" {
		return n
	}
	return fmt.Sprintf("profile %#04x", uint16(p))
}

// IsCD reports whether this profile is a CD rather than a DVD or BD. It is
// what decides whether raw 2352-byte sectors and audio tracks are possible
// at all: only a CD has them.
func (p Profile) IsCD() bool {
	return p == ProfileCDROM || p == ProfileCDR || p == ProfileCDRW
}

// IsWritable reports whether a disc of this kind can be written to, given a
// drive that supports it. A pressed CD-ROM cannot, whatever the drive.
func (p Profile) IsWritable() bool {
	switch p {
	case ProfileCDROM, ProfileDVDROM, ProfileBDROM, ProfileHDDVDROM, ProfileNone:
		return false
	}
	return true
}

// Capabilities probes the drive. It never fails because one probe was
// refused: a drive that answers neither question still yields its INQUIRY
// name, and what could not be asked is recorded in Notes.
func (d *Drive) Capabilities() (*Capabilities, error) {
	info, err := d.Inquiry()
	if err != nil {
		return nil, err
	}
	c := &Capabilities{Info: info}

	if err := d.readConfiguration(c); err != nil {
		if !IsUnsupported(err) && !IsNoMedium(err) {
			return nil, err
		}
		c.Notes = append(c.Notes, "this drive does not answer GET CONFIGURATION, so the list of disc kinds comes from its capabilities page instead")
	}
	if err := d.readCapabilitiesPage(c); err != nil {
		if !IsUnsupported(err) && !IsNoMedium(err) {
			return nil, err
		}
		c.Notes = append(c.Notes, "this drive does not answer the CD/DVD capabilities page, so its buffer size and speeds are unknown")
	}
	c.CurrentProfileName = c.CurrentProfile.Name()
	c.summarise()
	return c, nil
}

// readConfiguration walks the feature descriptors GET CONFIGURATION returns.
// The answer is self-describing and variable length: ask for the header
// first to learn how long the whole thing is, then ask again for that much.
func (d *Drive) readConfiguration(c *Capabilities) error {
	head := make([]byte, 8)
	if err := d.in(configCDB(len(head)), head, shortTimeout); err != nil {
		return err
	}
	// Data Length counts everything after its own four bytes.
	total := int(be32(head[0:4])) + 4
	if total <= 8 {
		return nil
	}
	if total > 1<<16 {
		total = 1 << 16
	}
	buf := make([]byte, total)
	if err := d.in(configCDB(total), buf, shortTimeout); err != nil {
		return err
	}
	c.CurrentProfile = Profile(be16(buf[6:8]))

	for p := 8; p+4 <= len(buf); {
		code := be16(buf[p : p+2])
		current := buf[p+2]&0x01 != 0
		addLen := int(buf[p+3])
		body := buf[p+4:]
		if addLen > len(body) {
			break
		}
		body = body[:addLen]
		c.applyFeature(uint16(code), current, body)
		p += 4 + addLen
	}
	return nil
}

func configCDB(n int) []byte {
	return []byte{opGetConfiguration, 0x00, 0, 0, 0, 0, 0, byte(n >> 8), byte(n), 0}
}

// Feature numbers, from MMC's feature table. Only the ones that say
// something a user would want to know are handled.
const (
	featProfileList         = 0x0000
	featCore                = 0x0001
	featRemovableMedium     = 0x0003
	featRandomReadable      = 0x0010
	featMultiRead           = 0x001d
	featCDRead              = 0x001e
	featDVDRead             = 0x001f
	featRandomWritable      = 0x0020
	featIncrementalWrite    = 0x0021
	featFormattable         = 0x0023
	featDefectManagement    = 0x0024
	featRestrictedOverwrite = 0x0026
	featCDRWCAVWrite        = 0x0028
	featMRW                 = 0x0029
	featDVDPlusRW           = 0x002a
	featDVDPlusR            = 0x002b
	featRigidOverwrite      = 0x002c
	featCDTrackAtOnce       = 0x002d
	featCDMastering         = 0x002e
	featDVDWrite            = 0x002f
	featBDRead              = 0x0040
	featBDWrite             = 0x0041
	featCDRWMediaWrite      = 0x0037
	featRealTimeStream      = 0x0107
	featDriveSerial         = 0x0108
)

func (c *Capabilities) applyFeature(code uint16, current bool, body []byte) {
	switch code {
	case featProfileList:
		for p := 0; p+4 <= len(body); p += 4 {
			c.Profiles = append(c.Profiles, Profile(be16(body[p:p+2])))
		}
		c.applyProfiles()
	case featRemovableMedium:
		if len(body) >= 1 {
			c.LoadingMechanism = loadingMechanism(body[0] >> 5)
			c.CanEject = body[0]&0x08 != 0
			c.CanLock = body[0]&0x01 != 0
		}
	case featMultiRead:
		c.IsMultiRead = true
	case featCDRead:
		c.Read.CD = true
		if len(body) >= 1 {
			c.CanReadCDText = body[0]&0x01 != 0
			c.CanReadC2Errors = body[0]&0x02 != 0
		}
	case featDVDRead:
		c.Read.DVD = true
		if len(body) >= 4 && body[2]&0x01 != 0 {
			c.Read.DualLayer = true
		}
	case featCDTrackAtOnce:
		c.CanWriteTAO = true
		if len(body) >= 2 {
			c.CanTestWrite = body[0]&0x04 != 0
			c.HasBurnProof = body[0]&0x40 != 0
			c.CanWriteRaw = body[0]&0x01 != 0 // RW raw
		}
	case featCDMastering:
		c.CanWriteSAO = true
		if len(body) >= 1 {
			c.CanWriteRaw = c.CanWriteRaw || body[0]&0x01 != 0
			c.HasBurnProof = c.HasBurnProof || body[0]&0x40 != 0
		}
	case featDVDWrite, featDVDPlusR, featDVDPlusRW, featIncrementalWrite,
		featRestrictedOverwrite, featRigidOverwrite, featCDRWCAVWrite,
		featCDRWMediaWrite, featRandomWritable, featBDWrite:
		// Covered in detail by the profile list; noted here so a drive that
		// omits a writable profile but advertises the feature is not shown
		// as read-only.
	case featDriveSerial:
		c.SerialNumber = serialNumber(body)
	}
}

// applyProfiles turns the profile list into the read and write matrices.
// The profile list is the drive's complete statement of the disc kinds it
// handles; a writable profile present means this drive can write that kind.
func (c *Capabilities) applyProfiles() {
	for _, p := range c.Profiles {
		r, w := &c.Read, &c.Write
		switch p {
		case ProfileCDROM:
			r.CD = true
		case ProfileCDR:
			r.CD, r.CDR, w.CDR = true, true, true
		case ProfileCDRW:
			r.CD, r.CDRW, w.CDRW = true, true, true
		case ProfileDVDROM:
			r.DVD = true
		case ProfileDVDRSeq:
			r.DVD, r.DVDR, w.DVD, w.DVDR = true, true, true, true
		case ProfileDVDRDLSeq, ProfileDVDRDLJump:
			r.DVD, r.DVDR, r.DualLayer = true, true, true
			w.DVD, w.DVDR, w.DualLayer = true, true, true
		case ProfileDVDRAM:
			r.DVD, r.DVDRAM, w.DVD, w.DVDRAM = true, true, true, true
		case ProfileDVDRWRO:
			r.DVD, r.DVDRW = true, true
		case ProfileDVDRWSeq, ProfileDVDRWDL:
			r.DVD, r.DVDRW, w.DVD, w.DVDRW = true, true, true, true
		case ProfileDVDPlusR:
			r.DVD, r.DVDPlusR, w.DVD, w.DVDPlusR = true, true, true, true
		case ProfileDVDPlusRDL:
			r.DVD, r.DVDPlusR, r.DualLayer = true, true, true
			w.DVD, w.DVDPlusR, w.DualLayer = true, true, true
		case ProfileDVDPlusRW:
			r.DVD, r.DVDPlusRW, w.DVD, w.DVDPlusRW = true, true, true, true
		case ProfileDVDPlusRWDL:
			r.DVD, r.DVDPlusRW, r.DualLayer = true, true, true
			w.DVD, w.DVDPlusRW, w.DualLayer = true, true, true
		case ProfileBDROM:
			r.BD = true
		case ProfileBDRSeq, ProfileBDRRandom:
			r.BD, r.BDR, w.BD, w.BDR = true, true, true, true
		case ProfileBDRE:
			r.BD, r.BDRE, w.BD, w.BDRE = true, true, true, true
		case ProfileHDDVDROM:
			r.HDDVD = true
		case ProfileHDDVDR, ProfileHDDVDRAM:
			r.HDDVD, w.HDDVD = true, true
		}
	}
}

// loadingMechanism names how a disc gets into the drive. The answers are
// names rather than sentences, for the same reason an Ability has one: a
// page shows them in whatever language it is in, and it needs something
// that does not change underneath a translation.
func loadingMechanism(t byte) string {
	switch t {
	case 0:
		return "caddy"
	case 1:
		return "tray"
	case 2:
		return "pop-up"
	case 4:
		return "changer-individual"
	case 5:
		return "changer-magazine"
	default:
		return "unknown"
	}
}

// readCapabilitiesPage reads mode page 0x2A. Long deprecated and still the
// only source for the buffer size, the speeds and the audio abilities.
func (d *Drive) readCapabilitiesPage(c *Capabilities) error {
	head := make([]byte, 8)
	if err := d.in(modeSenseCDB(0x2a, len(head)), head, shortTimeout); err != nil {
		return err
	}
	total := be16(head[0:2]) + 2
	if total <= 8 {
		return nil
	}
	if total > 4096 {
		total = 4096
	}
	buf := make([]byte, total)
	if err := d.in(modeSenseCDB(0x2a, total), buf, shortTimeout); err != nil {
		return err
	}
	// Skip the mode parameter header and any block descriptors.
	blockLen := be16(buf[6:8])
	p := 8 + blockLen
	if p+2 > len(buf) {
		return nil
	}
	page := buf[p:]
	if page[0]&0x3f != 0x2a {
		return nil
	}
	pageLen := int(page[1]) + 2
	if pageLen > len(page) {
		pageLen = len(page)
	}
	page = page[:pageLen]
	if len(page) < 12 {
		return nil
	}

	c.Read.CD = c.Read.CD || page[2]&0x01 != 0 || page[2]&0x02 != 0
	c.Read.CDR = c.Read.CDR || page[2]&0x01 != 0
	c.Read.CDRW = c.Read.CDRW || page[2]&0x02 != 0
	c.Read.DVD = c.Read.DVD || page[2]&0x08 != 0 || page[2]&0x10 != 0 || page[2]&0x20 != 0
	c.Read.DVDR = c.Read.DVDR || page[2]&0x10 != 0
	c.Read.DVDRAM = c.Read.DVDRAM || page[2]&0x20 != 0
	c.Write.CDR = c.Write.CDR || page[3]&0x01 != 0
	c.Write.CDRW = c.Write.CDRW || page[3]&0x02 != 0
	c.CanTestWrite = c.CanTestWrite || page[3]&0x04 != 0
	c.Write.DVDR = c.Write.DVDR || page[3]&0x10 != 0
	c.Write.DVDRAM = c.Write.DVDRAM || page[3]&0x20 != 0

	c.CanPlayAudio = page[4]&0x01 != 0
	c.CanReadMode2Form2 = page[4]&0x20 != 0
	c.IsMultiRead = c.IsMultiRead || page[4]&0x40 != 0
	c.HasBurnProof = c.HasBurnProof || page[4]&0x80 != 0
	// CD-DA Commands Supported is the bit that says READ CD will hand over
	// raw 2352-byte sectors - the one capability every audio rip and every
	// .img depends on.
	c.CanReadRawCD = c.CanReadRawCD || page[5]&0x01 != 0
	c.CanReadAccurateCDDA = page[5]&0x02 != 0
	c.CanReadSubchannel = page[5]&0x04 != 0
	c.CanReadC2Errors = c.CanReadC2Errors || page[5]&0x10 != 0
	c.CanReadISRC = page[5]&0x20 != 0
	c.CanReadUPC = page[5]&0x40 != 0

	if c.LoadingMechanism == "" {
		c.LoadingMechanism = loadingMechanism(page[6] >> 5)
	}
	c.CanEject = c.CanEject || page[6]&0x08 != 0
	c.CanLock = c.CanLock || page[6]&0x01 != 0

	c.MaxReadSpeedKB = be16(page[8:10])
	c.BufferKB = be16(page[12:14])
	if len(page) >= 16 {
		c.CurrentReadSpeedKB = be16(page[14:16])
	}
	if len(page) >= 20 {
		c.MaxWriteSpeedKB = be16(page[18:20])
	}
	// Write speed descriptors, four bytes each, at the end of the page.
	if len(page) >= 32 {
		n := be16(page[30:32])
		for i := 0; i < n && 32+i*4+4 <= len(page); i++ {
			if v := be16(page[32+i*4+2 : 32+i*4+4]); v > 0 {
				c.WriteSpeedsKB = append(c.WriteSpeedsKB, v)
			}
		}
		sort.Ints(c.WriteSpeedsKB)
	}
	c.CanSetReadSpeed = c.MaxReadSpeedKB > 0
	return nil
}

func modeSenseCDB(page byte, n int) []byte {
	return []byte{opModeSense10, 0, page, 0, 0, 0, 0, byte(n >> 8), byte(n), 0}
}

func trimmed(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, ch := range b {
		if ch >= 0x20 && ch < 0x7f {
			out = append(out, ch)
		}
	}
	return strings.TrimSpace(string(out))
}

// serialNumber cleans up what the drive serial feature returns. Drives that
// do not actually have one still answer the feature, with a body of padding
// and a byte or two of rubbish - one seen here reports ")U". Showing that as
// a serial number is worse than showing none, so an answer with fewer than
// four letters and digits in it is discarded: a manufacturing code is never
// that short.
func serialNumber(body []byte) string {
	s := trimmed(body)
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			n++
		}
	}
	if n < 4 {
		return ""
	}
	return s
}

// summarise phrases the flags as sentences. Every line the probe could
// decide lands in Can or in Cannot, which is what makes the UI's two
// columns meaningful: an empty Cannot means a drive that does everything
// asked of it, not a probe that gave up.
func (c *Capabilities) summarise() {
	c.Can, c.Cannot = nil, nil
	say := func(name string, ok bool, yes, no string) {
		if ok {
			c.Can = append(c.Can, Ability{Name: name, Text: yes})
		} else {
			c.Cannot = append(c.Cannot, Ability{Name: name, Text: no})
		}
	}

	say("readCD", c.Read.CD, "read CD-ROM and CD-R discs", "read CD discs")
	say("readCDRW", c.Read.CDRW, "read CD-RW discs", "read CD-RW discs")
	say("readDVD", c.Read.DVD, "read DVD discs", "read DVD discs")
	say("readBD", c.Read.BD, "read Blu-ray discs", "read Blu-ray discs")
	say("readRaw", c.CanReadRawCD, "read raw 2352-byte sectors, so audio tracks and exact disc images are possible",
		"read raw sectors, so only 2048-byte data sectors can be ripped - no audio, no raw image")
	say("readC2", c.CanReadC2Errors, "report C2 error pointers, so a rip can say which bytes it is unsure of",
		"report C2 error pointers, so a rip cannot tell a clean sector from a guessed one")
	say("accurateCDDA", c.CanReadAccurateCDDA, "return audio samples at the address asked for, so an audio rip needs no jitter correction",
		"guarantee audio samples land at the address asked for, so an audio rip may drift by a few samples between reads")
	say("mode2Form2", c.CanReadMode2Form2, "read Mode 2 Form 2 sectors, as used by Video CD and photo discs",
		"read Mode 2 Form 2 sectors, so Video CD and photo discs may rip short")
	say("cdText", c.CanReadCDText, "read CD-Text", "read CD-Text")
	say("isrc", c.CanReadISRC, "read track ISRC codes", "read track ISRC codes")
	say("upc", c.CanReadUPC, "read the disc's media catalogue number", "read the disc's media catalogue number")
	say("writeCDR", c.Write.CDR, "write CD-R discs", "write CD-R discs")
	say("writeCDRW", c.Write.CDRW, "write and erase CD-RW discs", "write CD-RW discs")
	say("writeDVDR", c.Write.DVDR || c.Write.DVDPlusR, "write recordable DVDs", "write recordable DVDs")
	say("writeDVDRW", c.Write.DVDRW || c.Write.DVDPlusRW || c.Write.DVDRAM, "write rewritable DVDs", "write rewritable DVDs")
	say("writeBD", c.Write.BD, "write Blu-ray discs", "write Blu-ray discs")
	say("sessionAtOnce", c.CanWriteSAO, "burn a disc in one pass (session at once), which is what an exact image copy needs",
		"burn session-at-once, so an image with its own layout cannot be written exactly")
	say("trackAtOnce", c.CanWriteTAO, "burn track at once", "burn track at once")
	say("testWrite", c.CanTestWrite, "run a burn with the laser off as a rehearsal", "rehearse a burn with the laser off")
	say("burnProof", c.HasBurnProof, "recover from a buffer underrun mid-burn", "recover from a buffer underrun, so a slow source can ruin a disc")
	say("eject", c.CanEject, "open and close its tray under software control", "open its tray under software control")
	say("multiRead", c.IsMultiRead, "read multi-session and packet-written discs", "read packet-written discs reliably")
}
