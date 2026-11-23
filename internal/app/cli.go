package app

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// shutdownRequests carries a graceful exit asked for by `literouter stop`. A
// channel instead of a signal because a second process cannot raise Ctrl+C in
// another one on Windows.
var shutdownRequests = make(chan struct{}, 1)

func usage() {
	fmt.Fprintf(os.Stderr, `literouter %s — 本地 OpenAI 兼容网关与会话交接台

用法：
  literouter                       后台启动；已经在跑就只报地址
  literouter up [--port N] [--open] [--restart]
                                   同上（显式写法）
  literouter serve [--port N] [--open] [--restart]
                                   前台运行（Ctrl+C 退出）
  literouter status                看后台实例的地址与进程号
  literouter stop                  停掉后台实例
  literouter version

日常只记两条：
  literouter         # 后台起服务，打印地址，回到提示符
  literouter stop    # 收工
`, version)
}

func upCmd(args []string) int {
	flags := flag.NewFlagSet("up", flag.ContinueOnError)
	port := flags.Int("port", 0, "转发给 serve 的监听端口（0 = 用配置文件）")
	restart := flags.Bool("restart", false, "重启已运行的实例")
	open := flags.Bool("open", false, "顺带打开浏览器")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if running, ok := liveInstance(); ok && !*restart {
		printRunning(running, "已经在运行")
		if *open {
			openBrowser(running.Address)
		}
		return 0
	}
	if *restart {
		if running, ok := liveInstance(); ok {
			if err := stopInstance(running, 5*time.Second); err != nil {
				fmt.Fprintln(os.Stderr, "literouter: "+err.Error())
				return 1
			}
			waitStopped(running.Address, 5*time.Second)
			clearState()
		}
	}

	if err := startBackground(*port); err != nil {
		fmt.Fprintln(os.Stderr, "literouter: "+err.Error())
		return 1
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if running, ok := liveInstance(); ok {
			printRunning(running, "已在后台运行")
			if *open {
				openBrowser(running.Address)
			}
			return 0
		}
		time.Sleep(150 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "literouter: 后台实例没有按时启动，试试 literouter serve 看报错")
	return 1
}

func statusCmd() int {
	running, ok := liveInstance()
	if !ok {
		fmt.Println("literouter 没有在运行。")
		return 1
	}
	fmt.Printf("literouter %s\n  界面  %s\n  进程  %d\n", running.Version, running.Address, running.PID)
	return 0
}

func stopCmd() int {
	running, ok := liveInstance()
	if !ok {
		fmt.Println("literouter 没有在运行。")
		return 0
	}
	if err := stopInstance(running, 5*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "literouter: "+err.Error())
		return 1
	}
	waitStopped(running.Address, 5*time.Second)
	clearState()
	fmt.Printf("已停止 %s（进程 %d）\n", running.Address, running.PID)
	return 0
}

func printRunning(running serverState, state string) {
	fmt.Printf("literouter %s %s\n  界面  %s\n  停止  literouter stop\n", running.Version, state, running.Address)
}

// startBackground re-executes this binary with `serve`. The state file makes
// this a one-way handoff: the parent does not need the child's PID.
func startBackground(port int) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"serve"}
	if port > 0 {
		args = append(args, "--port", strconv.Itoa(port))
	}
	command := exec.Command(self, args...)
	command.SysProcAttr = detachAttr()
	// The child does not inherit this terminal: closing the window leaves it
	// running, and its output goes to the log file instead.
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	return command.Start()
}

func openBrowser(address string) {
	var command string
	var args []string
	switch runtime.GOOS {
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", address}
	case "darwin":
		command, args = "open", []string{address}
	default:
		command, args = "xdg-open", []string{address}
	}
	_ = exec.Command(command, args...).Start()
}

func isPortBusy(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "already in use") || strings.Contains(message, "only one usage")
}
