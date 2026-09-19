// Package discfs is the vocabulary ripperX uses to talk about what is on a
// disc, whatever filesystem it was mastered with.
//
// There are two in practice. A CD is ISO 9660, usually with Joliet or Rock
// Ridge bolted on to carry the real names. A DVD is UDF - every DVD-Video,
// every game disc, most data DVDs written this century - and a lot of them
// carry no ISO 9660 at all, so a reader that knows only ISO 9660 says
// "there is no filesystem on this disc" about a disc that is full of files.
//
// The types live here rather than in either reader so that neither has to
// import the other, and so that the browse, rip and play paths are written
// once against the idea of a disc rather than twice against two formats.
package discfs

import (
	"io"
	"time"
)

// Volume is what the disc says about itself. Every field is as recorded,
// trimmed of the padding the formats insist on.
type Volume struct {
	// Format is the filesystem the rest of this came from: "ISO 9660" or
	// "UDF". It is shown, because it is the difference between a disc that
	// any machine will read and one that wants something from this century.
	Format string `json:"format,omitempty"`

	VolumeID    string    `json:"volumeId"`
	SystemID    string    `json:"systemId,omitempty"`
	VolumeSetID string    `json:"volumeSetId,omitempty"`
	Publisher   string    `json:"publisher,omitempty"`
	Preparer    string    `json:"preparer,omitempty"`
	Application string    `json:"application,omitempty"`
	Created     time.Time `json:"created,omitzero"`
	Modified    time.Time `json:"modified,omitzero"`
	Sectors     int64     `json:"sectors"`
	Bytes       int64     `json:"bytes"`

	// Which naming schemes this disc carries, which is worth showing: it is
	// the difference between a disc whose names survive a copy and one whose
	// names were flattened to 8.3 by its author. Neither applies to UDF,
	// which has had real names from the start.
	Joliet    bool `json:"joliet"`
	RockRidge bool `json:"rockRidge"`
}

// Entry is one file or directory. Path is always absolute within the disc
// and always uses forward slashes, whatever the disc was mastered on.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime,omitzero"`
	// Extent is the first sector of the file's contents. Shown because on a
	// scratched disc it is what says where in the disc a failure was.
	Extent int64 `json:"extent"`
	// Mode and SymlinkTarget come from Rock Ridge, and are empty on a disc
	// that has none.
	Mode          uint32 `json:"mode,omitempty"`
	SymlinkTarget string `json:"symlinkTarget,omitempty"`
	// ISOName is the plain 8.3 name, kept so a file can still be found by
	// the name a non-Joliet reader would show. UDF has no second name.
	ISOName string `json:"isoName,omitempty"`
}

// File is one file's contents. A UDF file can be scattered across the disc
// in several pieces, so this is an interface rather than the section of a
// reader an ISO 9660 file always is.
type File interface {
	io.ReaderAt
	io.ReadSeeker
}

// FS is an opened volume. Implementations hold no state beyond what they
// read at open time, so several requests may walk one at once.
type FS interface {
	Volume() Volume
	ReadDir(dir string) ([]Entry, error)
	Stat(p string) (Entry, error)
	Open(p string) (File, Entry, error)
	Walk(root string, fn func(Entry) error) error
}
