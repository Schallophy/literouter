package web

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The dashboard stays plain JavaScript, copied verbatim from the reference UI
// apart from branding. Its contract tests run against the embedded copies.
func TestUIAssetContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; UI asset contract requires Node.js")
	}
	files, err := filepath.Glob("selftest/*.test.mjs")
	if err != nil || len(files) == 0 {
		t.Fatalf("selftest files: %v", err)
	}
	args := append([]string{"--test"}, files...)
	output, err := exec.Command(node, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("UI asset contract failed: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
