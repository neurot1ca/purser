//go:build linux

package jobs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func processIdentity(pid int) (string, *uint64) {
	if pid <= 0 {
		return ProcessUnconfirmed, nil
	}
	fields, err := readProcStat(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if os.IsNotExist(err) {
		return ProcessDead, nil
	}
	if err != nil || len(fields) <= 19 {
		return ProcessUnconfirmed, nil
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return ProcessUnconfirmed, nil
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return ProcessDead, &start
	}
	return ProcessAlive, &start
}

func ProcessGroupStopped(pid int) bool {
	if pid <= 0 {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		fields, err := readProcStat(filepath.Join("/proc", entry.Name(), "stat"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || len(fields) <= 2 {
			return false
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return false
		}
		if group == pid && fields[0] != "Z" && fields[0] != "X" {
			return false
		}
	}
	return true
}

func readProcStat(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// comm is parenthesized but can contain spaces and parentheses.
	close := strings.LastIndexByte(string(data), ')')
	if close < 0 || close+2 > len(data) {
		return nil, syscall.EINVAL
	}
	return strings.Fields(string(data[close+2:])), nil
}
