package service

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// The upload's presigned URL stays valid for its TTL, so the file that
// is hashed and served is a copy made at finalize under a key no URL
// was issued for. Writing to the upload key afterwards changes nothing
// served; finalizing again replaces the copy and leaves nothing behind.
func TestFinalizeServesACopyTheUploadURLCannotReach(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	prod := &model.Product{Name: "Copy " + suffix, Slug: "copy-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	rel := &model.Release{ProductID: prod.ID, Version: "1.0.0", Channel: model.ReleaseChannelStable}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	fs := newFakeStorage()
	svc := NewReleaseService(ReleaseServiceConfig{Store: s, Storage: fs, Logger: slog.Default()})
	added, err := svc.AddArtifact(ctx, AddArtifactInput{ReleaseID: rel.ID, Platform: "darwin-arm64", Filename: "App.dmg"})
	if err != nil {
		t.Fatal(err)
	}
	uploadKey := added.Artifact.FileKey
	finalize := func(body string) *model.ReleaseArtifact {
		t.Helper()
		fs.objects[uploadKey] = []byte(body)
		a, err := svc.FinalizeArtifact(ctx, FinalizeArtifactInput{ArtifactID: added.Artifact.ID})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	first := finalize("original build")
	if first.FileKey == uploadKey || normalizeExt(first.FileKey) != ".dmg" || path.Dir(first.FileKey) != path.Dir(uploadKey) {
		t.Fatalf("served key = %q, want a key of its own beside the upload, with its extension (upload %q)", first.FileKey, uploadKey)
	}
	// Asked again with nothing new uploaded (a retry after a lost
	// answer): the artifact as that finalize made it.
	again, err := svc.FinalizeArtifact(ctx, FinalizeArtifactInput{ArtifactID: added.Artifact.ID, ExpectedSHA256: first.SHA256})
	if err != nil || again.FileKey != first.FileKey || again.SHA256 != first.SHA256 {
		t.Fatalf("finalize retried = %+v, %v; want the artifact the first one made", again, err)
	}
	if _, ok := fs.objects[uploadKey]; ok {
		t.Error("the upload was left behind after it was copied")
	}
	// The presigned URL is used again after finalize.
	fs.objects[uploadKey] = []byte("replaced after it was hashed")
	if got := fs.objects[first.FileKey]; !bytes.Equal(got, []byte("original build")) {
		t.Errorf("served bytes = %q, want the build that was hashed", got)
	}

	second := finalize("rebuilt")
	if second.FileKey == first.FileKey || !bytes.Equal(fs.objects[second.FileKey], []byte("rebuilt")) {
		t.Fatalf("finalizing again served %q from %q", fs.objects[second.FileKey], second.FileKey)
	}
	if _, ok := fs.objects[first.FileKey]; ok {
		t.Error("the copy an earlier finalize made was left behind")
	}
	if len(fs.objects) != 1 {
		t.Errorf("objects left = %d, want only the one served", len(fs.objects))
	}
}

// An upload with no extension is an object at its key, so its served
// copy cannot sit under that key (stores such as MinIO refuse an object
// inside another): it sits beside it.
func TestServedKeyForAnUploadWithoutExtension(t *testing.T) {
	upload := buildFileKey("p", "linux-x64", "1.0.0", "app")
	served := servedFileKey("p", "linux-x64", "1.0.0", normalizeExt(upload))
	if strings.HasPrefix(served, upload+"/") || path.Dir(served) != path.Dir(upload) {
		t.Fatalf("served %q for upload %q, want beside it", served, upload)
	}
	if got := buildFileKey("p", "linux-x64", "1.0.0", served); got != upload {
		t.Fatalf("upload key from the served one = %q, want %q", got, upload)
	}
}
