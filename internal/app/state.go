package app

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// serverState is what one running literouter writes down so the next invocation
// can find it instead of starting a second server on a second port.
type serverState struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
	// Token lets `literouter stop` ask the running instance to shut down. A page
	// cannot forge it: cross-origin requests cannot set custom headers without a
	// preflight, and the admin API returns no CORS headers.
	Token string `json:"token"`
}

// statePath is stable across working directories: the daily commands must find
// the same instance no matter where they are run from. LITEROUTER_STATE can
// point a test or a second deployment at its own file.
func statePath() string {
	if value := strings.TrimSpace(os.Getenv("LITEROUTER_STATE")); value != "" {
		return absoluteOr(value)
	}
	return filepath.Join(xdgHome("XDG_DATA_HOME", filepath.Join(".local", "share")), appName, "server.json")
}

func newToken() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return ""
	}
	return hex.EncodeToString(buffer)
}

func writeState(state serverState) error {
	path := statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	// Write a temp file and rename so a reader never sees half a JSON document.
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func readState() (serverState, bool) {
	data, err := os.ReadFile(statePath())
	if err != nil {
		return serverState{}, false
	}
	var state serverState
	if err := json.Unmarshal(data, &state); err != nil || state.Address == "" {
		return serverState{}, false
	}
	return state, true
}

func clearState() {
	_ = os.Remove(statePath())
}

// liveInstance returns the recorded server only when something there actually
// answers as literouter. The state file can outlive a killed process.
func liveInstance() (serverState, bool) {
	state, ok := readState()
	if !ok {
		return serverState{}, false
	}
	if !probeMeta(state.Address) {
		clearState()
		return serverState{}, false
	}
	return state, true
}

func probeMeta(address string) bool {
	client := &http.Client{Timeout: 600 * time.Millisecond}
	response, err := client.Get(address + "/api/meta")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var payload struct {
		App string `json:"app"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return false
	}
	return payload.App == appName
}

// stopInstance prefers the graceful path; a recorded instance that predates the
// shutdown endpoint, or one that stopped answering, falls back to killing the
// recorded PID.
func stopInstance(state serverState, timeout time.Duration) error {
	if state.Token != "" {
		if err := requestShutdown(state.Address, state.Token, timeout); err == nil {
			return nil
		}
	}
	process, err := os.FindProcess(state.PID)
	if err != nil {
		return fmt.Errorf("无法结束进程 %d：%w", state.PID, err)
	}
	if err := process.Kill(); err != nil {
		return fmt.Errorf("无法结束进程 %d：%w", state.PID, err)
	}
	return nil
}

func requestShutdown(address, token string, timeout time.Duration) error {
	body := bytes.NewReader([]byte("{}"))
	request, err := http.NewRequest("POST", address+"/api/shutdown", body)
	if err != nil {
		return err
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("x-literouter-token", token)
	client := &http.Client{Timeout: timeout}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("实例拒绝退出：" + response.Status)
	}
	return nil
}

func waitStopped(address string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !probeMeta(address) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
