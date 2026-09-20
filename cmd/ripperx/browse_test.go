package main

import "testing"

// A folder's size is the sum of everything under it, and the reason it is a
// separate request is that working it out costs a walk of the whole subtree.
// What matters is that the figure is right and that a bad path is answered
// rather than failing the whole request, since a listing is measured in one
// pass and one missing folder must not lose the other nine.
func TestMeasureDirAddsUpTheTree(t *testing.T) {
	fsys := testVolume(t)

	plan, err := planFiles(fsys, []string{"/"})
	if err != nil {
		t.Fatal(err)
	}
	got := measureDir(fsys, "/")
	if got.Error != "" {
		t.Fatalf("measuring the root: %s", got.Error)
	}
	if got.Bytes != plan.bytes {
		t.Errorf("the root holds %d bytes, and a rip of the same tree reads %d", got.Bytes, plan.bytes)
	}
	if got.Files != int64(len(plan.files)) {
		t.Errorf("%d files were counted, want %d", got.Files, len(plan.files))
	}

	// A file is its own size, so the page can ask about anything it lists.
	one := measureDir(fsys, "/A.TXT")
	if one.Error != "" || one.Files != 1 || one.Bytes <= 0 {
		t.Errorf("a single file measured as %+v", one)
	}

	// A path that is not there says so on its own row rather than failing
	// the request that carried it.
	missing := measureDir(fsys, "/nowhere")
	if missing.Error == "" {
		t.Error("a path that does not exist was measured without complaint")
	}
	if missing.Path != "/nowhere" {
		t.Errorf("the answer is about %q", missing.Path)
	}
}
