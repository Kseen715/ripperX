package udf

import (
	"errors"
	"io"
	"testing"
)

// A UDF file can be scattered across the disc, which is the case an
// io.SectionReader cannot express and the reason this package has a reader
// of its own.
func TestAFileInTwoPiecesReadsAsOne(t *testing.T) {
	fs := openTest(t, false)
	r, e, err := fs.Open("/sub/split.bin")
	if err != nil {
		t.Fatalf("opening the file: %v", err)
	}
	want := firstHalf + lastHalf
	if e.Size != int64(len(want)) {
		t.Fatalf("size = %d, want %d", e.Size, len(want))
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != want {
		t.Errorf("contents = %q, want %q", got, want)
	}

	// A read that starts inside the first piece and ends inside the second
	// is the one that goes wrong if anything does.
	across := make([]byte, 20)
	n, err := r.ReadAt(across, int64(len(firstHalf))-10)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("reading across the join: %v", err)
	}
	if string(across[:n]) != want[len(firstHalf)-10:len(firstHalf)+10] {
		t.Errorf("across the join = %q, want %q",
			across[:n], want[len(firstHalf)-10:len(firstHalf)+10])
	}
}
