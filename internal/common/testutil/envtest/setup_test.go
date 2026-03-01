package envtest

import (
	"os"
	"testing"
)

func TestFakeCRDsPath_ReturnsExistingDirectory(t *testing.T) {
	path, err := FakeCRDsPath()
	if err != nil {
		t.Fatalf("FakeCRDsPath() returned error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("FakeCRDsPath() returned %q, which does not exist: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("FakeCRDsPath() returned %q, which is not a directory", path)
	}
}

func TestFakeCRDSubDirs_ReturnsExistingDirectories(t *testing.T) {
	dirs, err := fakeCRDSubDirs()
	if err != nil {
		t.Fatalf("fakeCRDSubDirs() returned error: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("fakeCRDSubDirs() returned an empty slice; expected at least one CRD subdirectory")
	}

	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("fakeCRDSubDirs() includes %q, which does not exist: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("fakeCRDSubDirs() includes %q, which is not a directory", dir)
		}
	}
}
