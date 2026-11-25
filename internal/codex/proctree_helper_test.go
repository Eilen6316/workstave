package codex

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// descendants returns "name(pid)" for every process below root, from one CIM snapshot.
func descendants(t *testing.T, root int) []string {
	t.Helper()
	if root == 0 {
		return nil
	}
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"Get-CimInstance Win32_Process | ForEach-Object { '{0},{1},{2}' -f $_.ProcessId,$_.ParentProcessId,$_.Name }").Output()
	if err != nil {
		return nil
	}
	type proc struct {
		pid  int
		name string
	}
	kids := map[int][]proc{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) != 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		kids[ppid] = append(kids[ppid], proc{pid, f[2]})
	}
	var res []string
	queue := []int{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, k := range kids[cur] {
			res = append(res, k.name+"("+strconv.Itoa(k.pid)+")")
			queue = append(queue, k.pid)
		}
	}
	return res
}

func hasProc(tree []string, name string) bool {
	for _, s := range tree {
		if strings.HasPrefix(strings.ToLower(s), strings.ToLower(name)+"(") {
			return true
		}
	}
	return false
}
