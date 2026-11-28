package procgroup

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobs maps a started process id to the Job Object that contains its tree. The owner keeps the
// handle open for the life of the run. When the owner exits for any reason the operating system
// closes the handle, and JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE then terminates every member.
var jobs = struct {
	mu sync.Mutex
	m  map[int]windows.Handle
}{m: map[int]windows.Handle{}}

// Configure is a no-op on Windows; Job Object membership is established by Attach after Start.
func Configure(cmd *exec.Cmd) {}

// Attach places the started process pid in a new Job Object that kills all members when the
// owner's handle closes. Call it right after cmd.Start. A descendant created before Attach
// returns is not a member; that window is small but not zero. Call Release when the run is over.
func Attach(pid int) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("procgroup: create job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("procgroup: configure job object for pid %d: %w", pid, err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("procgroup: open pid %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("procgroup: assign pid %d to job object: %w", pid, err)
	}
	jobs.mu.Lock()
	jobs.m[pid] = job
	jobs.mu.Unlock()
	return nil
}

func takeJob(pid int) (windows.Handle, bool) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	job, ok := jobs.m[pid]
	delete(jobs.m, pid)
	return job, ok
}

// Release closes the Job Object of pid. Closing it terminates any member still running, so a
// run must be released only after its process has been waited for.
func Release(pid int) {
	if job, ok := takeJob(pid); ok {
		_ = windows.CloseHandle(job)
	}
}

// Kill terminates the Job Object of pid when one is attached, and otherwise removes pid and all
// its descendants with taskkill /T /F.
func Kill(pid int) error {
	if job, ok := takeJob(pid); ok {
		terminateErr := windows.TerminateJobObject(job, 1)
		closeErr := windows.CloseHandle(job)
		if terminateErr != nil {
			return fmt.Errorf("procgroup: terminate job object of pid %d: %w", pid, terminateErr)
		}
		if closeErr != nil {
			return fmt.Errorf("procgroup: close job object of pid %d: %w", pid, closeErr)
		}
		return nil
	}
	out, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("procgroup: taskkill pid %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}
