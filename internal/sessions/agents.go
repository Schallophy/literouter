package sessions

import (
	"os"
)

// detectAgents reports which agents have a store on this machine, so the UI can
// say "OpenCode 已找到 / Qoder 未安装" instead of silently listing nothing.
func (d *Detector) detectAgents() []Agent {
	return []Agent{
		{
			ID:      "opencode",
			Label:   "OpenCode",
			Store:   d.prettyPath(opencodeDBPath(d.home)),
			Present: fileExists(opencodeDBPath(d.home)) || isDir(joinHome(d.home, ".local", "share", "opencode")),
		},
		{
			ID:      "qoder",
			Label:   "Qoder",
			Store:   d.prettyPath(joinHome(d.home, ".qoder", "projects")),
			Present: isDir(joinHome(d.home, ".qoder", "projects")),
		},
		{
			ID:      "qoder-cn",
			Label:   "Qoder CN",
			Store:   d.prettyPath(joinHome(d.home, ".qoder-cn", "projects")),
			Present: isDir(joinHome(d.home, ".qoder-cn", "projects")),
		},
		{
			ID:      "workbuddy",
			Label:   "WorkBuddy",
			Store:   d.prettyPath(joinHome(d.home, ".workbuddy", "projects")),
			Present: isDir(joinHome(d.home, ".workbuddy", "projects")),
		},
		{
			ID:      "zcode",
			Label:   "ZCode",
			Store:   d.prettyPath(zcodeDBPath(d.home)),
			Present: fileExists(zcodeDBPath(d.home)) || isDir(joinHome(d.home, ".zcode", "cli")),
		},
	}
}

func isFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
