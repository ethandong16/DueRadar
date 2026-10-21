package instance

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirectoryOwnershipAndRelease(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	if second, err := Acquire(dir + string(os.PathSeparator) + "."); !errors.Is(err, ErrInUse) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("duplicate directory ownership: %v", err)
	}
	other, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatal("independent directory refused", err)
	}
	other.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal("close must be idempotent", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".dueradar.lock")); err != nil {
		t.Fatal("marker must remain after release", err)
	}
	reopened, err := Acquire(dir)
	if err != nil {
		t.Fatal("released directory still blocked", err)
	}
	reopened.Close()
}

func TestOwnershipAcrossProcessesAndCrash(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockProcessHelper$")
	child.Env = append(os.Environ(), "DUERADAR_TEST_LOCK_DIR="+dir)
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("child did not acquire directory: %q %v", line, err)
	}
	if lock, err := Acquire(dir); !errors.Is(err, ErrInUse) {
		if lock != nil {
			lock.Close()
		}
		t.Fatalf("another process obtained ownership: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	recovered, err := Acquire(dir)
	if err != nil {
		t.Fatal("process crash left stale ownership", err)
	}
	recovered.Close()
}

func TestLockProcessHelper(t *testing.T) {
	dir := os.Getenv("DUERADAR_TEST_LOCK_DIR")
	if dir == "" {
		return
	}
	lock, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fmt.Println("locked")
	io.Copy(io.Discard, os.Stdin)
}

func TestInvalidDirectoryDoesNotOpenData(t *testing.T) {
	if _, err := Acquire(""); err == nil {
		t.Fatal("empty directory accepted")
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("regular file accepted as directory")
	}
	contents, _ := os.ReadFile(path)
	if string(contents) != "preserve" {
		t.Fatal("rejected directory modified existing file")
	}
}

func TestDirectoryAliasSharesOwnership(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	lock, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skip("directory symlinks are not available:", err)
	}
	if other, err := Acquire(alias); !errors.Is(err, ErrInUse) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("directory alias bypassed ownership: %v", err)
	}
}
