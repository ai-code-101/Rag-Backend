package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalPutIsGroupReadable(t *testing.T) {
	root := t.TempDir()
	l := &Local{Root: root}
	if _, err := l.Put(context.Background(), "bots/b1/d1/file.txt", strings.NewReader("hello"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "bots", "b1", "d1", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Fatalf("file mode = %o, want 640", got)
	}
	di, err := os.Stat(filepath.Join(root, "bots", "b1", "d1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got&0o050 != 0o050 {
		t.Fatalf("dir mode = %o, want group r-x", got)
	}
}
