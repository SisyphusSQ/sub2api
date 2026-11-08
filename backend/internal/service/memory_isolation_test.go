package service

import (
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// 内存指标统计整个进程，独立进程排除其他测试遗留的后台分配。
func runMemoryTestInSubprocess(t *testing.T) bool {
	t.Helper()
	const envKey = "SUB2API_MEMORY_TEST"
	if os.Getenv(envKey) == t.Name() {
		return false
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), envKey+"="+t.Name())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated memory test failed: %v\n%s", err, output)
	}
	return true
}
