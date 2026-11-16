//go:build windows

package conpty

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fakeJobAttributes struct {
	owner   *fakeJobOperations
	keys    []uintptr
	job     windows.Handle
	console windows.Handle
}

func (a *fakeJobAttributes) Update(key uintptr, value unsafe.Pointer, size uintptr) error {
	a.keys = append(a.keys, key)
	if key == procThreadAttributeJobList {
		if size != unsafe.Sizeof(windows.Handle(0)) {
			return errors.New("wrong job attribute size")
		}
		a.job = *(*windows.Handle)(value)
		return a.owner.step("job-list")
	}
	return errors.New("unexpected pointer-valued attribute")
}
func (a *fakeJobAttributes) List() *windows.ProcThreadAttributeList {
	return &windows.ProcThreadAttributeList{}
}
func (a *fakeJobAttributes) Delete() { a.owner.events = append(a.owner.events, "attributes-delete") }

type fakeJobOperations struct {
	fail       string
	events     []string
	attributes *fakeJobAttributes
	closed     []windows.Handle
	killed     []windows.Handle
	created    int
	resumed    int
	flags      uint32
}

func (f *fakeJobOperations) step(name string) error {
	f.events = append(f.events, name)
	if f.fail == name {
		return errors.New("injected " + name)
	}
	return nil
}
func (f *fakeJobOperations) operations() conPTYJobOperations {
	f.attributes = &fakeJobAttributes{owner: f}
	return conPTYJobOperations{
		newJob: func() (windows.Handle, error) {
			if err := f.step("new-job"); err != nil {
				return 0, err
			}
			return 101, nil
		},
		protectJob: func(h windows.Handle) error {
			if h != 101 {
				panic("wrong job")
			}
			return f.step("private-job")
		},
		limitJob: func(h windows.Handle) error {
			if h != 101 {
				panic("wrong job")
			}
			return f.step("limits")
		},
		attributes: func() (conPTYAttributes, error) {
			if err := f.step("attributes"); err != nil {
				return nil, err
			}
			return f.attributes, nil
		},
		attachConsole: func(attributes conPTYAttributes, console windows.Handle) error {
			if attributes != f.attributes {
				panic("console attribute used another list")
			}
			f.attributes.keys = append(f.attributes.keys, windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE)
			f.attributes.console = console
			return f.step("console")
		},
		create: func(_, _, _, _ *uint16, si *windows.StartupInfoEx, flags uint32) (windows.ProcessInformation, error) {
			f.created++
			f.flags = flags
			if !reflect.DeepEqual(f.attributes.keys, []uintptr{windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, procThreadAttributeJobList}) || f.attributes.job != 101 || f.attributes.console != 88 || si.ProcThreadAttributeList == nil {
				panic("process creation did not carry both exact attributes")
			}
			if err := f.step("create"); err != nil {
				return windows.ProcessInformation{}, err
			}
			return windows.ProcessInformation{Process: 201, Thread: 202, ProcessId: 301}, nil
		},
		member: func(process, job windows.Handle) (bool, error) {
			if process != 201 || job != 101 {
				panic("membership lookup used another owner")
			}
			if err := f.step("membership"); err != nil {
				return false, err
			}
			return true, nil
		},
		resume: func(h windows.Handle) (uint32, error) {
			if h != 202 {
				panic("wrong thread")
			}
			f.resumed++
			return 1, f.step("resume")
		},
		terminate: func(h windows.Handle, _ uint32) error { f.killed = append(f.killed, h); return nil },
		close:     func(h windows.Handle) error { f.closed = append(f.closed, h); return nil },
		wait: func(h windows.Handle) (uint32, error) {
			if h != 201 {
				panic("wait reopened/replaced process")
			}
			return windows.WAIT_OBJECT_0, nil
		},
		exitCode: func(h windows.Handle) (uint32, error) {
			if h != 201 {
				panic("exit code used another process")
			}
			return 7, nil
		},
	}
}
func startFakeJob(t *testing.T, f *fakeJobOperations) (*ownedConPTYProcess, error) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return startConPTYJobWithOperations(88, t.TempDir(), self, []string{"unexecuted-unit-argument"}, []string{"SystemRoot=C:\\Windows"}, f.operations())
}

func TestConPTYJobRefusesEveryUncontainedLaunch(t *testing.T) {
	for _, step := range []string{"new-job", "private-job", "limits", "attributes", "console", "job-list", "create", "membership", "resume"} {
		t.Run(step, func(t *testing.T) {
			f := &fakeJobOperations{fail: step}
			process, err := startFakeJob(t, f)
			if err == nil || process != nil {
				t.Fatal("failed ownership setup returned a live process")
			}
			afterCreate := step == "membership" || step == "resume"
			if afterCreate && !reflect.DeepEqual(f.killed, []windows.Handle{201}) {
				t.Fatalf("partial process not stopped by its original handle: %v", f.killed)
			}
			if !afterCreate && len(f.killed) != 0 {
				t.Fatalf("killed a process without an owned create result: %v", f.killed)
			}
			if step != "resume" && f.resumed != 0 {
				t.Fatal("thread resumed before ownership was established")
			}
			if step != "create" && !afterCreate && f.created != 0 {
				t.Fatal("uncontained fallback attempted process creation")
			}
			if step != "new-job" {
				jobsClosed := 0
				for _, h := range f.closed {
					if h == 101 {
						jobsClosed++
					}
				}
				if jobsClosed != 1 {
					t.Fatalf("job close count %d, handles %v", jobsClosed, f.closed)
				}
			}
		})
	}
}
func TestConPTYJobRetainsExactHandleAndPrivateLifetime(t *testing.T) {
	f := &fakeJobOperations{}
	process, err := startFakeJob(t, f)
	if err != nil {
		t.Fatal(err)
	}
	want := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED)
	if f.flags != want || f.resumed != 1 || process.pid != 301 {
		t.Fatalf("creation flags=%x resumed=%d pid=%d", f.flags, f.resumed, process.pid)
	}
	if !reflect.DeepEqual(f.closed, []windows.Handle{202}) {
		t.Fatalf("host lost its job/process handle: %v", f.closed)
	}
	if code, err := process.wait(); err != nil || code != 7 {
		t.Fatalf("wait code=%d err=%v", code, err)
	}
	if err := process.closeJob(); err != nil {
		t.Fatal(err)
	}
	if err := process.closeJob(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.closed, []windows.Handle{202, 201, 101}) {
		t.Fatalf("ownership handles were closed or reused incorrectly: %v", f.closed)
	}
	if len(f.killed) != 0 {
		t.Fatal("ordinary process wait used destructive PID cleanup")
	}
}
func TestConPTYJobEnvironmentPreservesWindowsValues(t *testing.T) {
	env, err := conPTYEnvironmentBlock([]string{"Path=old", "PATH=new", "UNICODE=\u03A9", "=C:=C:\\work"})
	if err != nil {
		t.Fatal(err)
	}
	decoded := string(utf16.Decode(env))
	if decoded != "=C:=C:\\work\x00PATH=new\x00UNICODE=\u03A9\x00\x00" {
		t.Fatalf("environment block %q", decoded)
	}
	for _, bad := range []string{"NO_EQUALS", "BAD=one\x00two"} {
		if _, err := conPTYEnvironmentBlock([]string{bad}); err == nil {
			t.Fatalf("invalid environment accepted: %q", strings.ReplaceAll(bad, "\x00", "<NUL>"))
		}
	}
}

func TestConPTYPseudoConsoleAttributeUsesHandleValue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    uintptr
		lastError error
		wantError error
	}{
		{"success-ignores-last-error", 1, windows.ERROR_ACCESS_DENIED, nil},
		{"native-failure", 0, windows.ERROR_INVALID_PARAMETER, windows.ERROR_INVALID_PARAMETER},
		{"missing-last-error", 0, syscall.Errno(0), windows.ERROR_INVALID_FUNCTION},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := new(windows.ProcThreadAttributeList)
			// Deliberately not a Go pointer. Every handle bit must reach Win32.
			console := windows.Handle(^uintptr(0) - 0x123)
			called := 0
			err := attachConPTYConsole(list, console, func(args ...uintptr) (uintptr, uintptr, error) {
				called++
				want := []uintptr{uintptr(unsafe.Pointer(list)), 0, windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(console), unsafe.Sizeof(console), 0, 0}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("attribute arguments=%x want=%x", args, want)
				}
				return tc.result, 0, tc.lastError
			})
			if called != 1 || !errors.Is(err, tc.wantError) {
				t.Fatalf("calls=%d err=%v want=%v", called, err, tc.wantError)
			}
		})
	}
}
