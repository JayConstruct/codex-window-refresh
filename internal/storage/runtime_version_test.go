package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeVersionPublicationRequiresCurrentStoreOwnership(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	old, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.PublishRuntimeVersion([]byte(`{"version":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	data := []byte(`{"version":"new"}`)
	if err := next.PublishRuntimeVersion(data); err != nil {
		t.Fatal(err)
	}
	if err := old.PublishRuntimeVersion([]byte(`{"version":"old"}`)); err == nil {
		t.Fatal("closed owner published over the replacement")
	}
	actual, err := os.ReadFile(filepath.Join(dir, RuntimeVersionFile))
	if err != nil || string(actual) != string(data) {
		t.Fatal("replacement receipt changed", err)
	}
}
