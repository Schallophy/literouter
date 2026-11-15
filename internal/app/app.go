// Package app wires configuration loading, the HTTP server, hot reload and
// signal handling into the literouter process.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Schallophy/literouter/internal/config"
	"github.com/Schallophy/literouter/internal/httpapi"
	gateway "github.com/Schallophy/literouter/internal/runtime"
	"github.com/Schallophy/literouter/internal/util"
)

const version = httpapi.Version

// Run boots the gateway and returns the process exit code.
func Run() int {
	loadDotEnv(".env")

	configPath := envPath("LITEROUTER_CONFIG", "config.json")
	dataDir := envPath("LITEROUTER_DATA_DIR", "data")

	loaded, err := config.LoadConfig(configPath)
	if err != nil {
		level := util.ParseLogLevel(os.Getenv("LITEROUTER_LOG_LEVEL"))
		fallback := util.NewLogger(level, filepath.Join(dataDir, "literouter.log"), 2*1024*1024, 3)
		var configErr *config.ConfigError
		if ok := asConfigError(err, &configErr); ok {
			fallback.Error(configErr.Message, nil)
			for _, detail := range configErr.Details {
				fallback.Error("  - "+detail, nil)
			}
			_ = fallback.Flush()
			return 1
		}
		fallback.Error(err.Error(), nil)
		_ = fallback.Flush()
		return 1
	}

	overrides := config.ApplyEnvOverrides(loaded.Config)
	loaded.Config = overrides.Config
	logFile := strings.TrimSpace(loaded.Config.Log.File)
	if logFile == "" {
		logFile = filepath.Join(dataDir, "literouter.log")
	} else {
		absolute, absErr := filepath.Abs(logFile)
		if absErr == nil {
			logFile = absolute
		}
	}
	logLevel := util.ParseLogLevel(os.Getenv("LITEROUTER_LOG_LEVEL"))
	if _, present := os.LookupEnv("LITEROUTER_LOG_LEVEL"); !present {
		logLevel = loaded.Config.Log.Level
	}
	logger := util.NewLogger(logLevel, logFile, loaded.Config.Log.MaxBytes, loaded.Config.Log.MaxFiles)

	if loaded.Created {
		logger.Info("已生成默认配置文件", map[string]any{"path": configPath})
	}

	warnings := append([]string(nil), loaded.Warnings...)
	if !loaded.Persisted {
		warnings = append(warnings, loaded.Migrations...)
	}
	warnings = append(warnings, overrides.Warnings...)

	rt, err := gateway.Create(gateway.Options{
		ConfigPath: configPath,
		DataDir:    dataDir,
		Config:     loaded.Config,
		Logger:     logger,
		Warnings:   warnings,
	})
	if err != nil {
		logger.Error(err.Error(), nil)
		_ = logger.Flush()
		return 1
	}

	handler := httpapi.NewServer(rt, logger)
	cfg := rt.ConfigSnapshot()
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "in use") {
			logger.Error(fmt.Sprintf("端口 %d 已被占用，请修改 config.json 中的 port", cfg.Port), nil)
		} else {
			logger.Error(err.Error(), nil)
		}
		rt.Shutdown()
		_ = logger.Flush()
		return 1
	}
	server := &http.Server{Handler: handler}
	serveErrors := make(chan error, 1)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			serveErrors <- serveErr
		}
	}()

	banner(rt)
	if loaded.Persisted {
		logger.Info("配置已迁移为新格式，写回 "+configPath, nil)
	}
	for _, warning := range rt.Warnings() {
		logger.Warn(warning, nil)
	}
	rt.Bootstrap()

	var closing sync.Mutex
	isClosing := false
	watcher := newConfigWatcher(configPath, 400*time.Millisecond, 250*time.Millisecond, func() {
		closing.Lock()
		closed := isClosing
		closing.Unlock()
		if closed {
			return
		}
		reloadConfig(rt, logger)
	})
	if !watchDisabled() {
		watcher.Start()
	}

	signals := make(chan os.Signal, 4)
	notifySignals(signals)
	shutdownDone := make(chan int, 2)
	go func() {
		for {
			select {
			case sig := <-signals:
				name := signalName(sig)
				if name == "SIGHUP" {
					reloadConfig(rt, logger)
					continue
				}
				closing.Lock()
				alreadyClosing := isClosing
				isClosing = true
				closing.Unlock()
				if alreadyClosing {
					logger.Warn(name+" 重复到达，立即退出", nil)
					rt.Shutdown()
					_ = logger.Flush()
					shutdownDone <- 1
					return
				}
				watcher.Stop()
				logger.Info("收到 "+name+"，正在关闭", nil)
				// Keep consuming signals while the drain runs so a second signal
				// can force an immediate exit.
				go func() { shutdownDone <- gracefulShutdown(server, rt, logger) }()
			case err := <-serveErrors:
				logger.Error("server error", map[string]any{"error": err.Error()})
				watcher.Stop()
				rt.Shutdown()
				_ = logger.Flush()
				shutdownDone <- 1
				return
			}
		}
	}()

	code := <-shutdownDone
	return code
}

func gracefulShutdown(server *http.Server, rt *gateway.Runtime, logger *util.Logger) int {
	timedOut := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(ctx)
		timedOut <- err != nil
	}()
	forced := false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var shutdownErr bool
	select {
	case failed := <-timedOut:
		shutdownErr = failed
	case <-deadline.C:
		forced = true
	}
	if forced || shutdownErr {
		logger.Warn("等待连接关闭超时，强制退出", nil)
		rt.Shutdown()
		_ = logger.Flush()
		return 1
	}
	rt.Shutdown()
	logger.Info("已关闭", nil)
	_ = logger.Flush()
	return 0
}

func reloadConfig(rt *gateway.Runtime, logger *util.Logger) {
	path := rt.ConfigPath()
	if _, err := os.Stat(path); err != nil {
		// 删除或保存过程中的短暂缺失：保留当前配置。
		return
	}
	next, err := config.LoadConfig(path)
	if err != nil {
		logger.Error("配置热加载失败，继续使用当前配置", map[string]any{"error": err.Error()})
		return
	}
	current := rt.ConfigSnapshot()
	nextJSON, _ := json.Marshal(next.Config)
	currentJSON, _ := json.Marshal(current)
	if string(nextJSON) == string(currentJSON) {
		return
	}
	warnings, err := rt.ApplyConfig(next.Config, false)
	if err != nil {
		logger.Error("配置热加载失败，继续使用当前配置", map[string]any{"error": err.Error()})
		return
	}
	logger.Info("配置已重新加载（文件变更）", nil)
	if next.Persisted {
		logger.Info("迁移后的配置已写回", nil)
	}
	for _, warning := range append(next.Warnings, warnings...) {
		logger.Warn(warning, nil)
	}
}

func watchDisabled() bool {
	value := os.Getenv("LITEROUTER_CONFIG_WATCH")
	return value == "0" || value == "false"
}

type configWatcher struct {
	path     string
	interval time.Duration
	debounce time.Duration
	onChange func()

	stopCh  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	timer   *time.Timer
	lastMod time.Time
	lastLen int64
	missing bool
}

func newConfigWatcher(path string, interval, debounce time.Duration, onChange func()) *configWatcher {
	return &configWatcher{path: path, interval: interval, debounce: debounce, onChange: onChange, stopCh: make(chan struct{})}
}

func (w *configWatcher) Start() {
	info, err := os.Stat(w.path)
	if err == nil {
		w.lastMod = info.ModTime()
		w.lastLen = info.Size()
	} else {
		w.missing = true
	}
	go w.loop()
}

func (w *configWatcher) Stop() {
	w.once.Do(func() {
		close(w.stopCh)
		w.mu.Lock()
		if w.timer != nil {
			w.timer.Stop()
			w.timer = nil
		}
		w.mu.Unlock()
	})
}

func (w *configWatcher) loop() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			info, err := os.Stat(w.path)
			if err != nil {
				w.mu.Lock()
				w.missing = true
				w.mu.Unlock()
				continue
			}
			w.mu.Lock()
			changed := w.missing || !info.ModTime().Equal(w.lastMod) || info.Size() != w.lastLen
			if changed {
				w.missing = false
				w.lastMod = info.ModTime()
				w.lastLen = info.Size()
				if w.timer != nil {
					w.timer.Stop()
				}
				w.timer = time.AfterFunc(w.debounce, func() {
					w.mu.Lock()
					w.timer = nil
					w.mu.Unlock()
					select {
					case <-w.stopCh:
						return
					default:
					}
					w.onChange()
				})
			}
			w.mu.Unlock()
		}
	}
}

func banner(rt *gateway.Runtime) {
	cfg := rt.ConfigSnapshot()
	host := cfg.Host
	if host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	base := fmt.Sprintf("http://%s:%d", host, cfg.Port)
	admin := base
	if !cfg.UI.Enabled {
		admin = "已关闭（ui.enabled = false）"
	}
	configLine := rt.ConfigPath()
	if !cfg.UI.Enabled {
		configLine += "  ← 唯一配置入口"
	}
	rows := []string{
		paint("38;5;179", "◈ literouter") + " " + paint("38;5;240", "v"+version),
		row("gateway", base+"/v1"),
		row("admin", admin),
		row("config", configLine),
	}
	fmt.Fprintln(os.Stdout, strings.Join(rows, "\n"))
}

func row(label, value string) string {
	return paint("38;5;240", fmt.Sprintf("  %-9s", label)) + value
}

func paint(code, text string) string {
	if !colorEnabled() {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func colorEnabled() bool {
	if _, exists := os.LookupEnv("NO_COLOR"); exists {
		return false
	}
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func envPath(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		value = fallback
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return value
	}
	return absolute
}

func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		index := strings.Index(line, "=")
		if index <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:index])
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		value := strings.TrimSpace(line[index+1:])
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		_ = os.Setenv(key, value)
	}
}

func notifySignals(channel chan<- os.Signal) {
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if runtime.GOOS == "windows" {
		// SIGBREAK (21) 是 Windows 控制台 Ctrl+Break；直接写数字保持跨平台可编译。
		signals = append(signals, syscall.Signal(21))
	} else {
		signals = append(signals, syscall.Signal(1)) // SIGHUP
	}
	signal.Notify(channel, signals...)
}

func signalName(sig os.Signal) string {
	switch sig {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	if runtime.GOOS == "windows" && sig == syscall.Signal(21) {
		return "SIGBREAK"
	}
	if runtime.GOOS != "windows" && sig == syscall.Signal(1) {
		return "SIGHUP"
	}
	return sig.String()
}

func asConfigError(err error, target **config.ConfigError) bool {
	for err != nil {
		if typed, ok := err.(*config.ConfigError); ok {
			*target = typed
			return true
		}
		type unwrapper interface{ Unwrap() error }
		next, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = next.Unwrap()
	}
	return false
}
