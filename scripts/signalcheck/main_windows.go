//go:build windows

// Command signalcheck exercises literouter's console signal handling.
// It is an acceptance helper, not part of the shipped binary.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	createNewConsole      = 0x00000010
	createNewProcessGroup = 0x00000200
	ctrlCEvent            = 0
	ctrlBreakEvent        = 1
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole        = kernel32.NewProc("AttachConsole")
	procFreeConsole          = kernel32.NewProc("FreeConsole")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	procGenerateConsoleCtrlEvent = kernel32.NewProc("GenerateConsoleCtrlEvent")
)

func main() {
	binary := flag.String("bin", "", "literouter binary")
	config := flag.String("config", "", "config path")
	dataDir := flag.String("data", "", "data dir")
	port := flag.Int("port", 8907, "gateway port")
	mode := flag.String("mode", "break", "break|int|force|duplicate")
	flag.Parse()

	flags := uint32(createNewConsole)
	if *mode == "break" || *mode == "force" || *mode == "duplicate" {
		flags |= createNewProcessGroup
	}
	command := exec.Command(*binary, "serve")
	command.Env = append(os.Environ(),
		"LITEROUTER_CONFIG="+*config,
		"LITEROUTER_DATA_DIR="+*dataDir,
		"LITEROUTER_STATE="+*config+".state.json",
	)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	logFile, err := os.Create(*config + ".run.log")
	if err != nil {
		fail("create log: %v", err)
	}
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		fail("start: %v", err)
	}
	pid := command.Process.Pid
	started := time.Now()

	waitHealthy(*port)

	var blocker net.Conn
	if *mode == "force" || *mode == "duplicate" {
		blocker = openBlockedRequest(*port)
		defer blocker.Close()
	}

	attach(pid)
	defer freeConsole()
	switch *mode {
	case "int":
		sendCtrl(ctrlCEvent, 0)
	case "break":
		sendCtrl(ctrlBreakEvent, uintptr(pid))
	case "force":
		sendCtrl(ctrlBreakEvent, uintptr(pid))
	case "duplicate":
		sendCtrl(ctrlBreakEvent, uintptr(pid))
		time.Sleep(200 * time.Millisecond)
		sendCtrl(ctrlBreakEvent, uintptr(pid))
	}
	freeConsole()

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			fail("wait: %v", err)
		}
		fmt.Printf("mode=%s exit=%d elapsed=%.2fs\n", *mode, code, time.Since(started).Seconds())
		log, _ := os.ReadFile(*config + ".run.log")
		fmt.Printf("--- log tail ---\n%s", tail(string(log), 600))
	case <-time.After(12 * time.Second):
		_ = command.Process.Kill()
		fail("process did not exit within 12s")
	}
}

func waitHealthy(port int) {
	client := http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	fail("gateway did not become healthy")
}

func openBlockedRequest(port int) net.Conn {
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fail("dial: %v", err)
	}
	_, err = fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nContent-Length: 1000000\r\n\r\n{\"model\":\"auto\",")
	if err != nil {
		fail("write: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	return conn
}

func attach(pid int) {
	procFreeConsole.Call()
	ok, _, err := procAttachConsole.Call(uintptr(pid))
	if ok == 0 {
		fail("AttachConsole: %v", err)
	}
	procSetConsoleCtrlHandler.Call(0, 1)
}

func freeConsole() {
	procFreeConsole.Call()
}

func sendCtrl(event, group uintptr) {
	ok, _, err := procGenerateConsoleCtrlEvent.Call(event, group)
	if ok == 0 {
		fail("GenerateConsoleCtrlEvent(%d,%d): %v", event, group, err)
	}
}

func tail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return "..." + text[len(text)-limit:]
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
