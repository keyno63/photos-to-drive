package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindDuplicateGroups(t *testing.T) {
	files := []remoteFile{
		{Path: "0/a.heic", Size: 100, MD5: "ABC"},
		{Path: "2/b.heic", Size: 100, MD5: "abc"},
		{Path: "unique.mov", Size: 200, MD5: "def"},
		{Path: "google-document", Size: 0},
	}
	groups, missing := findDuplicateGroups(files)
	if missing != 1 || len(groups) != 1 {
		t.Fatalf("missing=%d groups=%v", missing, groups)
	}
	if got := strings.Join(groups[0].Paths, ","); got != "0/a.heic,2/b.heic" {
		t.Fatalf("paths=%s", got)
	}
	var out bytes.Buffer
	printDuplicates(&out, "test", files)
	if !strings.Contains(out.String(), "Duplicate groups: 1") || !strings.Contains(out.String(), "reclaimable if one copy per group is kept: 100 B") {
		t.Fatalf("output=%s", out.String())
	}
}

func TestPrepareUpload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photo file.heic")
	data := []byte("photo-data")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareUpload(path)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.File.Close()
	h := md5.Sum(data)
	wantHash := hex.EncodeToString(h[:])
	if prepared.MD5 != wantHash || prepared.Name != wantHash+"-photo file.heic" || prepared.Size != int64(len(data)) {
		t.Fatalf("prepared=%+v", prepared)
	}
	b := make([]byte, len(data))
	if _, err = prepared.File.Read(b); err != nil || !bytes.Equal(b, data) {
		t.Fatalf("upload reader was not rewound: %q, %v", b, err)
	}
	if err = sourceUnchanged(prepared); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareUploadRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jpg")
	link := filepath.Join(dir, "link.jpg")
	if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if prepared, err := prepareUpload(link); err == nil {
		prepared.File.Close()
		t.Fatal("accepted symlink")
	}
}

func TestExpandHomeLeavesOrdinaryPath(t *testing.T) {
	if got := expandHome("/tmp/client.json"); got != "/tmp/client.json" {
		t.Fatal(got)
	}
}
