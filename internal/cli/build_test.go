package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveBuildContext(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "message.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "ignored"), []byte("large history"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive, err := archiveBuildContext(root, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		contents, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(contents)
	}
	if files["Dockerfile"] != "FROM scratch\n" || files["message.txt"] != "hello" {
		t.Fatalf("archive contents = %#v", files)
	}
	if _, exists := files[".git/ignored"]; exists {
		t.Fatal(".git directory was included")
	}
}

func TestArchiveBuildContextRejectsDockerfileOutsideContext(t *testing.T) {
	if _, err := archiveBuildContext(t.TempDir(), "../Dockerfile"); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
}
