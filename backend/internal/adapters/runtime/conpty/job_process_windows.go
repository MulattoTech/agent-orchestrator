//go:build windows

package conpty

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows SDK defines ProcThreadAttributeJobList as attribute 13 with
// PROC_THREAD_ATTRIBUTE_INPUT. x/sys has not exported this constant yet.
// https://learn.microsoft.com/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute
const procThreadAttributeJobList = uintptr(0x0002000d)

type conPTYAttributes interface {
	Update(uintptr, unsafe.Pointer, uintptr) error
	List() *windows.ProcThreadAttributeList
	Delete()
}

// The operations are private and explicitly supplied only by focused tests.
// Production always uses nativeConPTYJobOperations; there is no fallback that
// launches an uncontained process when any job/attribute operation fails.
type conPTYJobOperations struct {
	newJob        func() (windows.Handle, error)
	protectJob    func(windows.Handle) error
	limitJob      func(windows.Handle) error
	attributes    func() (conPTYAttributes, error)
	attachConsole func(conPTYAttributes, windows.Handle) error
	create        func(*uint16, *uint16, *uint16, *uint16, *windows.StartupInfoEx, uint32) (windows.ProcessInformation, error)
	member        func(windows.Handle, windows.Handle) (bool, error)
	resume        func(windows.Handle) (uint32, error)
	terminate     func(windows.Handle, uint32) error
	close         func(windows.Handle) error
	wait          func(windows.Handle) (uint32, error)
	exitCode      func(windows.Handle) (uint32, error)
}

var isProcessInJobProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
var updateProcThreadAttributeProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")

// PSEUDOCONSOLE takes HPCON itself as lpValue, unlike pointer-valued JOB_LIST.
// Keep the opaque handle an integer at the syscall boundary: converting it to
// unsafe.Pointer would invent a Go pointer and retain it in x/sys's pointer
// container. Passing &console instead would change the native ABI.
// https://learn.microsoft.com/windows/console/creating-a-pseudoconsole-session
func attachConPTYConsole(list *windows.ProcThreadAttributeList, console windows.Handle, update func(...uintptr) (uintptr, uintptr, error)) error {
	result, _, err := update(uintptr(unsafe.Pointer(list)), 0, windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(console), unsafe.Sizeof(console), 0, 0)
	runtime.KeepAlive(list)
	if result != 0 {
		return nil
	}
	if err == nil || err == syscall.Errno(0) {
		return windows.ERROR_INVALID_FUNCTION
	}
	return err
}

func nativeConPTYJobOperations() conPTYJobOperations {
	return conPTYJobOperations{
		newJob: func() (windows.Handle, error) { return windows.CreateJobObject(nil, nil) },
		protectJob: func(job windows.Handle) error {
			return windows.SetHandleInformation(job, windows.HANDLE_FLAG_INHERIT, 0)
		},
		limitJob: func(job windows.Handle) error {
			limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
			limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
			_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
			return err
		},
		attributes: func() (conPTYAttributes, error) { return windows.NewProcThreadAttributeList(2) },
		attachConsole: func(attributes conPTYAttributes, console windows.Handle) error {
			return attachConPTYConsole(attributes.List(), console, updateProcThreadAttributeProc.Call)
		},
		create: func(application, command, environment, directory *uint16, startup *windows.StartupInfoEx, flags uint32) (windows.ProcessInformation, error) {
			var pi windows.ProcessInformation
			// nil security attributes and bInheritHandles=false keep the sole job
			// handle in the host. The console is carried by its explicit attribute.
			err := windows.CreateProcess(application, command, nil, nil, false, flags, environment, directory, &startup.StartupInfo, &pi)
			return pi, err
		},
		member: func(process, job windows.Handle) (bool, error) {
			var member int32
			result, _, err := isProcessInJobProc.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&member)))
			if result == 0 {
				if err == syscall.Errno(0) {
					err = windows.ERROR_INVALID_FUNCTION
				}
				return false, err
			}
			return member != 0, nil
		},
		resume:    windows.ResumeThread,
		terminate: windows.TerminateProcess,
		close:     windows.CloseHandle,
		wait: func(process windows.Handle) (uint32, error) {
			return windows.WaitForSingleObject(process, windows.INFINITE)
		},
		exitCode: func(process windows.Handle) (uint32, error) {
			var code uint32
			err := windows.GetExitCodeProcess(process, &code)
			return code, err
		},
	}
}

type ownedConPTYProcess struct {
	process    windows.Handle
	pid        int
	job        windows.Handle
	operations conPTYJobOperations
	closeOnce  sync.Once
	closeErr   error
}

func (p *ownedConPTYProcess) closeJob() error {
	p.closeOnce.Do(func() { p.closeErr = p.operations.close(p.job) })
	return p.closeErr
}

func (p *ownedConPTYProcess) wait() (int, error) {
	// This is the original CreateProcess handle, never a later PID lookup.
	defer p.operations.close(p.process)
	state, err := p.operations.wait(p.process)
	if err != nil {
		return 1, err
	}
	if state != windows.WAIT_OBJECT_0 {
		return 1, fmt.Errorf("conpty: unexpected process wait state %d", state)
	}
	code, err := p.operations.exitCode(p.process)
	return int(code), err
}

func startConPTYJob(console windows.Handle, cwd, command string, args, env []string) (*ownedConPTYProcess, error) {
	return startConPTYJobWithOperations(console, cwd, command, args, env, nativeConPTYJobOperations())
}

func startConPTYJobWithOperations(console windows.Handle, cwd, command string, args, env []string, ops conPTYJobOperations) (*ownedConPTYProcess, error) {
	if console == 0 {
		return nil, errors.New("conpty: missing console handle")
	}
	directory, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("conpty: working directory: %w", err)
	}
	executable := command
	if !filepath.IsAbs(executable) && (strings.ContainsAny(executable, "/\\") || filepath.VolumeName(executable) != "") {
		executable = filepath.Join(directory, executable)
	}
	executable, err = exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("conpty: resolve executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	application, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{command}, args...)))
	if err != nil {
		return nil, err
	}
	directoryUTF16, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return nil, err
	}
	environment, err := conPTYEnvironmentBlock(env)
	if err != nil {
		return nil, err
	}

	job, err := ops.newJob()
	if err != nil {
		return nil, fmt.Errorf("conpty: create owned job: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = ops.close(job)
		}
	}()
	if err = ops.protectJob(job); err != nil {
		return nil, fmt.Errorf("conpty: make job handle private: %w", err)
	}
	if err = ops.limitJob(job); err != nil {
		return nil, fmt.Errorf("conpty: contain process tree: %w", err)
	}
	attributes, err := ops.attributes()
	if err != nil {
		return nil, fmt.Errorf("conpty: create launch attributes: %w", err)
	}
	jobs := []windows.Handle{job}
	defer func() { attributes.Delete(); runtime.KeepAlive(jobs) }()
	if err = ops.attachConsole(attributes, console); err != nil {
		return nil, fmt.Errorf("conpty: attach console attribute: %w", err)
	}
	if err = attributes.Update(procThreadAttributeJobList, unsafe.Pointer(&jobs[0]), unsafe.Sizeof(jobs[0])); err != nil {
		return nil, fmt.Errorf("conpty: attach owned job attribute: %w", err)
	}
	startup := windows.StartupInfoEx{}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESTDHANDLES
	startup.ProcThreadAttributeList = attributes.List()
	// Atomic JOB_LIST assignment contains the child before any instruction.
	// CREATE_SUSPENDED also permits verification before releasing its thread.
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED)
	pi, err := ops.create(application, commandLine, &environment[0], directoryUTF16, &startup, flags)
	runtime.KeepAlive(environment)
	if err != nil {
		return nil, fmt.Errorf("conpty: create contained process: %w", err)
	}
	defer ops.close(pi.Thread)
	accepted := false
	defer func() {
		if !accepted {
			// Only the original handle returned by this invocation is terminated.
			// This also covers an unexpected missing job membership while suspended.
			_ = ops.terminate(pi.Process, 1)
			_ = ops.close(pi.Process)
		}
	}()
	if pi.Process == 0 || pi.Thread == 0 || pi.ProcessId == 0 {
		return nil, errors.New("conpty: incomplete process identity")
	}
	member, err := ops.member(pi.Process, job)
	if err != nil || !member {
		return nil, fmt.Errorf("conpty: verify contained process: %w", errors.Join(err, errors.New("owned job membership unverified")))
	}
	previous, err := ops.resume(pi.Thread)
	if err != nil || previous != 1 {
		return nil, fmt.Errorf("conpty: resume contained process: %w", errors.Join(err, fmt.Errorf("suspend count %d", previous)))
	}
	accepted, committed = true, true
	return &ownedConPTYProcess{process: pi.Process, pid: int(pi.ProcessId), job: job, operations: ops}, nil
}

func conPTYEnvironmentBlock(env []string) ([]uint16, error) {
	values := make(map[string]string, len(env))
	for _, entry := range env {
		if strings.ContainsRune(entry, 0) {
			return nil, errors.New("conpty: environment contains NUL")
		}
		equal := strings.IndexByte(entry, '=')
		if strings.HasPrefix(entry, "=") {
			if next := strings.IndexByte(entry[1:], '='); next >= 0 {
				equal = next + 1
			}
		}
		if equal <= 0 {
			return nil, errors.New("conpty: invalid environment entry")
		}
		values[strings.ToUpper(entry[:equal])] = entry
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := make([]string, 0, len(keys))
	for _, key := range keys {
		ordered = append(ordered, values[key])
	}
	return utf16.Encode([]rune(strings.Join(ordered, "\x00") + "\x00\x00")), nil
}
