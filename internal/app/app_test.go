package app

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestConfigWatcherDetectsRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	watcher := newConfigWatcher(path, 40*time.Millisecond, 250*time.Millisecond, func() {
		atomic.AddInt32(&calls, 1)
	})
	watcher.Start()
	defer watcher.Stop()

	if err := os.WriteFile(path, []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(path, future, future)
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&calls) > 0 })
}

func TestConfigWatcherDebouncesBursts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	watcher := newConfigWatcher(path, 30*time.Millisecond, 250*time.Millisecond, func() {
		atomic.AddInt32(&calls, 1)
	})
	watcher.Start()
	defer watcher.Stop()

	for index := 0; index < 5; index++ {
		payload := []byte(`{"version":` + string(rune('1'+index)) + `}`)
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Duration(index+1) * 100 * time.Millisecond)
		_ = os.Chtimes(path, future, future)
		time.Sleep(50 * time.Millisecond)
	}
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&calls) > 0 })
	time.Sleep(400 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("burst should debounce to one reload, got %d", got)
	}
}

func TestConfigWatcherRecoversMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "later.json")
	var calls int32
	watcher := newConfigWatcher(path, 40*time.Millisecond, 100*time.Millisecond, func() {
		atomic.AddInt32(&calls, 1)
	})
	watcher.Start()
	defer watcher.Stop()

	time.Sleep(120 * time.Millisecond)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("missing file must not trigger reload")
	}
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&calls) > 0 })
}

func TestConfigWatcherStopHaltsCallbacks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int32
	watcher := newConfigWatcher(path, 30*time.Millisecond, 100*time.Millisecond, func() {
		atomic.AddInt32(&calls, 1)
	})
	watcher.Start()
	watcher.Stop()
	if err := os.WriteFile(path, []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("stopped watcher fired %d times", got)
	}
}

func TestLoadDotEnvWithoutOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# comment\n" +
		"LITEROUTER_TEST_ENV_A=plain\n" +
		"export LITEROUTER_TEST_ENV_B=\"quoted value\"\n" +
		"LITEROUTER_TEST_ENV_C='single'\n" +
		"LITEROUTER_TEST_ENV_D=from-file\n" +
		"INVALID LINE\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LITEROUTER_TEST_ENV_D", "from-process")
	defer os.Unsetenv("LITEROUTER_TEST_ENV_A")
	defer os.Unsetenv("LITEROUTER_TEST_ENV_B")
	defer os.Unsetenv("LITEROUTER_TEST_ENV_C")

	loadDotEnv(path)
	if got := os.Getenv("LITEROUTER_TEST_ENV_A"); got != "plain" {
		t.Fatalf("A = %q", got)
	}
	if got := os.Getenv("LITEROUTER_TEST_ENV_B"); got != "quoted value" {
		t.Fatalf("B = %q", got)
	}
	if got := os.Getenv("LITEROUTER_TEST_ENV_C"); got != "single" {
		t.Fatalf("C = %q", got)
	}
	if got := os.Getenv("LITEROUTER_TEST_ENV_D"); got != "from-process" {
		t.Fatalf("existing env must win, got %q", got)
	}
}

func TestWatchDisabledFlag(t *testing.T) {
	t.Setenv("LITEROUTER_CONFIG_WATCH", "0")
	if !watchDisabled() {
		t.Fatal("0 should disable watching")
	}
	t.Setenv("LITEROUTER_CONFIG_WATCH", "false")
	if !watchDisabled() {
		t.Fatal("false should disable watching")
	}
	t.Setenv("LITEROUTER_CONFIG_WATCH", "1")
	if watchDisabled() {
		t.Fatal("1 should keep watching enabled")
	}
}
