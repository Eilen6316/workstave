package procgroup

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Configure is a no-op on Windows; taskkill walks the parent/child tree itself.
func Configure(cmd *exec.Cmd) {}

// Kill removes pid and all its descendants with taskkill /T /F.
func Kill(pid int) error {
	out, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("procgroup: taskkill pid %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}
