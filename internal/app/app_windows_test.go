//go:build windows

package app

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var procGetShortPathNameW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetShortPathNameW")

func shortPath(t *testing.T, path string) string {
	t.Helper()
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("utf16: %v", err)
	}
	buffer := make([]uint16, 1024)
	length, _, callErr := procGetShortPathNameW.Call(
		uintptr(unsafe.Pointer(pointer)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
	if length == 0 {
		t.Fatalf("GetShortPathName: %v", callErr)
	}
	return syscall.UTF16ToString(buffer[:length])
}

func TestConfigWatcherWorksUnderShortPath(t *testing.T) {
	base := os.Getenv("TEMP")
	if base == "" {
		base = os.Getenv("TMP")
	}
	if base == "" {
		t.Skip("no TEMP directory")
	}
	dir, err := os.MkdirTemp(base, "literouter watcher test ")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)

	shortDir := shortPath(t, dir)
	t.Logf("long=%s short=%s", dir, shortDir)
	if !strings.Contains(shortDir, "~") {
		t.Skip("8.3 name generation unavailable on this volume")
	}
	path := filepath.Join(shortDir, "config.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var calls int32
	watcher := newConfigWatcher(path, 40*time.Millisecond, 250*time.Millisecond, func() {
		atomic.AddInt32(&calls, 1)
	})
	watcher.Start()
	defer watcher.Stop()

	if err := os.WriteFile(path, []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(path, future, future)
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&calls) > 0 })
}
