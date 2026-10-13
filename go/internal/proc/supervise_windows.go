//go:build windows

package proc

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

const (
	jobObjectLimitKillOnJobClose = 0x00002000
	jobObjectExtendedLimitInfo   = 9
	jobObjectBasicAccounting     = 1
	processSetQuota              = 0x0100
	processTerminate             = 0x0001
)

var jobKernel = syscall.NewLazyDLL("kernel32.dll")
var createJobObject = jobKernel.NewProc("CreateJobObjectW")
var setJobInformation = jobKernel.NewProc("SetInformationJobObject")
var assignProcessToJob = jobKernel.NewProc("AssignProcessToJobObject")
var terminateJob = jobKernel.NewProc("TerminateJobObject")
var queryJobInformation = jobKernel.NewProc("QueryInformationJobObject")

type jobBasicLimits struct {
	PerProcessUserTimeLimit, PerJobUserTimeLimit int64
	LimitFlags                                   uint32
	MinimumWorkingSetSize, MaximumWorkingSetSize uintptr
	ActiveProcessLimit                           uint32
	Affinity                                     uintptr
	PriorityClass, SchedulingClass               uint32
}

type jobIOCounters struct{ Read, Write, Other, ReadBytes, WriteBytes, OtherBytes uint64 }
type jobExtendedLimits struct {
	Basic                                    jobBasicLimits
	IO                                       jobIOCounters
	ProcessMemoryLimit, JobMemoryLimit       uintptr
	PeakProcessMemoryUsed, PeakJobMemoryUsed uintptr
}
type jobAccounting struct {
	TotalUserTime, TotalKernelTime            int64
	PeriodUserTime, PeriodKernelTime          int64
	TotalPageFaults, TotalProcesses           uint32
	ActiveProcesses, TotalTerminatedProcesses uint32
}

type ProcessJob struct {
	handle      syscall.Handle
	ownerInside bool
}

func NewProcessJob() (*ProcessJob, error) {
	h, _, callErr := createJobObject.Call(0, 0)
	if h == 0 {
		return nil, callErr
	}
	job := &ProcessJob{handle: syscall.Handle(h)}
	limits := jobExtendedLimits{}
	limits.Basic.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, callErr := setJobInformation.Call(h, jobObjectExtendedLimitInfo,
		uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
	if r == 0 {
		_ = job.Close()
		return nil, callErr
	}
	return job, nil
}

func ContainCurrentProcess() (*ProcessJob, error) {
	job, err := NewProcessJob()
	if err != nil {
		return nil, err
	}
	if err := job.Assign(os.Getpid()); err != nil {
		_ = job.Close()
		return nil, err
	}
	return job, nil
}

func (j *ProcessJob) Assign(pid int) error {
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	r, _, callErr := assignProcessToJob.Call(uintptr(j.handle), uintptr(h))
	if r == 0 {
		return callErr
	}
	if pid == os.Getpid() {
		j.ownerInside = true
	}
	return nil
}

func (j *ProcessJob) Terminate(exitCode uint32) error {
	r, _, callErr := terminateJob.Call(uintptr(j.handle), uintptr(exitCode))
	if r == 0 {
		return callErr
	}
	return nil
}

// ConfirmDescendantsStopped reports true only after the root exits and no descendants remain.
func (j *ProcessJob) ConfirmDescendantsStopped(rootExited bool) bool {
	if !rootExited {
		return false
	}
	confirmed, _ := j.ConfirmStopped(time.Second)
	return confirmed
}

// ConfirmStopped preserves query errors and counts the owner only when assigned.
func (j *ProcessJob) ConfirmStopped(timeout time.Duration) (bool, error) {
	until := time.Now().Add(timeout)
	for time.Now().Before(until) {
		var info jobAccounting
		r, _, callErr := queryJobInformation.Call(uintptr(j.handle), jobObjectBasicAccounting,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
		if r == 0 {
			return false, callErr
		}
		allowed := uint32(0)
		if j.ownerInside {
			allowed = 1
		}
		if info.ActiveProcesses == allowed {
			return true, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false, errors.New("job teardown confirmation timed out")
}

type supervisionContainment struct{ job, bootstrap *ProcessJob }

func newSupervisionContainment() (supervisionContainment, string, error) {
	job, err := NewProcessJob()
	if err != nil {
		return supervisionContainment{}, "", err
	}
	current, err := syscall.GetCurrentProcess()
	if err != nil {
		_ = job.Close()
		return supervisionContainment{}, "", err
	}
	var handle syscall.Handle
	if err := syscall.DuplicateHandle(current, job.handle, current, &handle, 0, true, syscall.DUPLICATE_SAME_ACCESS); err != nil {
		_ = job.Close()
		return supervisionContainment{}, "", err
	}
	return supervisionContainment{job: job, bootstrap: &ProcessJob{handle: handle}}, strconv.FormatUint(uint64(handle), 10), nil
}
func (c supervisionContainment) attach(command *exec.Cmd) {
	command.SysProcAttr.AdditionalInheritedHandles = append(command.SysProcAttr.AdditionalInheritedHandles, c.bootstrap.handle)
}
func (c supervisionContainment) releaseBootstrap() {
	if c.bootstrap != nil {
		_ = c.bootstrap.Close()
	}
}
func (c supervisionContainment) owned() bool { return true }
func (c supervisionContainment) close() {
	c.releaseBootstrap()
	if c.job != nil {
		_ = c.job.Close()
	}
}
func (c supervisionContainment) stop() (bool, error) {
	if err := c.job.Terminate(1); err != nil {
		return false, err
	}
	return c.job.ConfirmStopped(3 * time.Second)
}
func configureGuard(command *exec.Cmd) { Hide(command) }

func inheritWorkerStreams(command *exec.Cmd, launch *Launch) (func(), error) {
	launch.Streams = [3]uintptr{}
	var handles []syscall.Handle
	cleanup := func() {
		for _, handle := range handles {
			_ = syscall.CloseHandle(handle)
		}
	}
	current, err := syscall.GetCurrentProcess()
	if err != nil {
		return cleanup, err
	}
	for i, file := range [3]*os.File{launch.Stdin, launch.Stdout, launch.Stderr} {
		if file == nil {
			continue
		}
		var handle syscall.Handle
		if err := syscall.DuplicateHandle(current, syscall.Handle(file.Fd()), current, &handle, 0, true, syscall.DUPLICATE_SAME_ACCESS); err != nil {
			cleanup()
			return func() {}, err
		}
		handles = append(handles, handle)
		launch.Streams[i] = uintptr(handle)
		command.SysProcAttr.AdditionalInheritedHandles = append(command.SysProcAttr.AdditionalInheritedHandles, handle)
	}
	return cleanup, nil
}
func nativeError(n uint64) error { return syscall.Errno(n) }
func errorNumber(err error) uint64 {
	var n syscall.Errno
	if errors.As(err, &n) {
		return uint64(n)
	}
	return 0
}
func processStopped(pid int) bool {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.Errno(87))
	}
	defer syscall.CloseHandle(h)
	result, err := syscall.WaitForSingleObject(h, 0)
	return err == nil && result == syscall.WAIT_OBJECT_0
}
func anchorMain() int    { return 2 }
func prepareSupervisor() {}
func guardMain(config supervisorConfig) int {
	h, err := strconv.ParseUint(config.Job, 10, 64)
	if err != nil {
		sendEvent(supervisionEvent{Kind: "stopped"}, err)
		return 1
	}
	// The unnamed job's inherited bootstrap handle is closed before spawning
	// any worker. Only the owner keeps a long-lived, non-inherited handle.
	job := &ProcessJob{handle: syscall.Handle(h)}
	err = job.Assign(os.Getpid())
	closeErr := job.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		sendEvent(supervisionEvent{Kind: "stopped"}, err)
		return 1
	}
	sendEvent(supervisionEvent{Kind: "ready"}, nil)
	var launch Launch
	if err := json.NewDecoder(os.Stdin).Decode(&launch); err != nil {
		return 1
	}
	runWorker(launch)
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
	return 0
}

func (j *ProcessJob) Close() error {
	if j.handle == 0 {
		return nil
	}
	err := syscall.CloseHandle(j.handle)
	j.handle = 0
	return err
}
