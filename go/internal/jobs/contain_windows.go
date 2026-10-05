//go:build windows

package jobs

import (
	"os"
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

type ProcessJob struct{ handle syscall.Handle }

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
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		var info jobAccounting
		r, _, _ := queryJobInformation.Call(uintptr(j.handle), jobObjectBasicAccounting,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
		if r == 0 {
			return false
		}
		if info.ActiveProcesses <= 1 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func (j *ProcessJob) Close() error {
	if j.handle == 0 {
		return nil
	}
	err := syscall.CloseHandle(j.handle)
	j.handle = 0
	return err
}
