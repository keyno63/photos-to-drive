package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssetUUID(t *testing.T) {
	uuid := "000931D7-9902-4CE1-B2DB-9111B04F7327"
	for _, path := range []string{"0/" + uuid + ".heic", "0/" + uuid + "_3.mov"} {
		got, ok := assetUUID(path)
		if !ok || got != uuid {
			t.Fatalf("assetUUID(%q) = %q, %v", path, got, ok)
		}
	}
	if _, ok := assetUUID("not-an-asset.jpg"); ok {
		t.Fatal("accepted invalid asset name")
	}
	got, ok := photosAssetUUID(uuid + "/L0/001")
	if !ok || got != uuid {
		t.Fatalf("photosAssetUUID = %q, %v", got, ok)
	}
}

func TestPhotosReportGroupsLivePhotoResources(t *testing.T) {
	o, original := fixture(t)
	uuid := "000931D7-9902-4CE1-B2DB-9111B04F7327"
	still := filepath.Join(filepath.Dir(original), uuid+".heic")
	if err := os.Rename(original, still); err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := run(context.Background(), o, backend(t, false, &n), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(filepath.Dir(original), uuid+"_3.mov")
	if err := os.WriteFile(video, []byte("pending-live-photo-video"), 0600); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(filepath.Dir(o.work), "photos.csv")
	o.photosReport = report
	reader := func(context.Context) ([]photoMetadata, error) {
		return []photoMetadata{{ID: uuid + "/L0/001", Filename: "IMG_1234.HEIC", Title: "旅行"}}, nil
	}
	var out bytes.Buffer
	if err := showPhotosReport(context.Background(), o, &out, reader); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "partially verified: 1") {
		t.Fatalf("unexpected summary: %s", out.String())
	}
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"IMG_1234.HEIC", "旅行", "partially_verified", uuid + ".heic", uuid + "_3.mov"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q: %s", want, text)
		}
	}
}
