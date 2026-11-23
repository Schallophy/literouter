// Package app wires configuration loading, the HTTP server, hot reload and
// signal handling into the literouter process.
package app

import (
	"context"
	"encoding/json"
	"flag"
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

// Run parses argv and returns the process exit code. The default action is the
// background server: the gateway is a daily tool, not a demo you keep a terminal
// open for. `literouter stop` closes it.
func Run() int {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve", "ui":
			return serve(os.Args[2:])
		case "up", "start":
			return upCmd(os.Args[2:])
		case "status":
			return statusCmd()
		case "stop", "quit":
			return stopCmd()
		case "version", "--version", "-v":
			fmt.Println("literouter", version)
			return 0
		case "help", "--help", "-h":
			usage()
			return 0
		default:
			if !strings.HasPrefix(os.Args[1], "-") {
				fmt.Fprintf(os.Stderr, "literouter: 未知命令 %q\n\n", os.Args[1])
				usage()
				return 2
			}
		}
	}
	return upCmd(os.Args[1:])
}

// serve runs the gateway in the foreground. It also records the instance in
// server.json so `literouter stop` can end it.
func serve(args []string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := flags.Int("port", 0, "监听端口（0 = 使用配置）")
	open := flags.Bool("open", false, "启动后打开浏览器")
	restart := flags.Bool("restart", false, "重启已运行的实例")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	// Repeating the command must not pile up servers on fallback ports.
	if running, ok := liveInstance(); ok {
		if !*restart {
			printRunning(running, "已经在运行")
			if *open {
				openBrowser(running.Address)
			}
			return 0
		}
		if err := stopInstance(running, 5*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "literouter: "+err.Error())
			return 1
		}
		waitStopped(running.Address, 5*time.Second)
		clearState()
	}

	loadDotEnv(".env")

	configPath, dataDir := resolvePaths()
	// A deployment keeps its keys beside its config; the working directory's
	// .env still wins, since loadDotEnv never overwrites what is set.
	loadDotEnv(filepath.Join(filepath.Dir(configPath), ".env"))

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
	if *port > 0 {
		loaded.Config.Port = *port
	}
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
	listener, actualPort, err := listenFor(cfg.Host, cfg.Port)
	if err != nil {
		if isPortBusy(err) {
			logger.Error(fmt.Sprintf("端口 %d 已被占用，请修改 config.json 中的 port", cfg.Port), nil)
		} else {
			logger.Error(err.Error(), nil)
		}
		rt.Shutdown()
		_ = logger.Flush()
		return 1
	}
	if actualPort != cfg.Port {
		logger.Warn(fmt.Sprintf("端口 %d 已被占用，改用 %d", cfg.Port, actualPort), nil)
	}
	displayHost := advertisedHost(cfg.Host)
	address := fmt.Sprintf("http://%s", net.JoinHostPort(displayHost, strconv.Itoa(actualPort)))
	handler.SetAddress(displayHost, actualPort)
	token := newToken()
	handler.Token = token
	handler.Stop = func() {
		select {
		case shutdownRequests <- struct{}{}:
		default:
		}
	}
	server := &http.Server{Handler: handler}
	serveErrors := make(chan error, 1)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			serveErrors <- serveErr
		}
	}()

	if err := writeState(serverState{
		Address: address, Port: actualPort, PID: os.Getpid(), Version: version, Token: token,
	}); err != nil {
		logger.Warn("无法记录运行地址："+err.Error(), nil)
	}
	// Remove our record on exit, but only when it still points at this instance.
	defer func() {
		if current, ok := readState(); ok && current.Address == address {
			clearState()
		}
	}()

	banner(rt, address)
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
	beginShutdown := func(name string) {
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
		// Keep consuming signals while the drain runs so a second signal can
		// force an immediate exit.
		go func() { shutdownDone <- gracefulShutdown(server, rt, logger) }()
	}
	go func() {
		for {
			select {
			case sig := <-signals:
				name := signalName(sig)
				if name == "SIGHUP" {
					reloadConfig(rt, logger)
					continue
				}
				beginShutdown(name)
			case <-shutdownRequests:
				beginShutdown("远程停止")
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

func banner(rt *gateway.Runtime, address string) {
	cfg := rt.ConfigSnapshot()
	admin := address
	if !cfg.UI.Enabled {
		admin = "已关闭（ui.enabled = false）"
	}
	configLine := rt.ConfigPath()
	if !cfg.UI.Enabled {
		configLine += "  ← 唯一配置入口"
	}
	rows := []string{
		paint("38;5;179", "◈ literouter") + " " + paint("38;5;240", "v"+version),
		row("gateway", address+"/v1"),
		row("admin", admin),
		row("config", configLine),
		row("data", rt.DataDir()),
		row("stop", "literouter stop"),
	}
	fmt.Fprintln(os.Stdout, strings.Join(rows, "\n"))
}

// listenFor binds the configured port, or asks the OS for a free one when it is
// already taken. A busy port must not keep the user away from the dashboard:
// the recorded address carries the real port.
func listenFor(host string, port int) (net.Listener, int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err == nil {
		return listener, portOf(listener.Addr()), nil
	}
	if !isPortBusy(err) {
		return nil, 0, err
	}
	listener, fallbackErr := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if fallbackErr != nil {
		return nil, 0, err
	}
	return listener, portOf(listener.Addr()), nil
}

func portOf(address net.Addr) int {
	_, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return 0
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return value
}

func advertisedHost(host string) string {
	switch host {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::", "[::]":
		return "::1"
	}
	return host
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
