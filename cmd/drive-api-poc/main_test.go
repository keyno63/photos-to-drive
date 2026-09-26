package main

import (
	"bytes"
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

func TestExpandHomeLeavesOrdinaryPath(t *testing.T) {
	if got := expandHome("/tmp/client.json"); got != "/tmp/client.json" {
		t.Fatal(got)
	}
}
