//go:build linux

package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func ProcessIdentity(pid int) (string, *uint64) {
	if pid <= 0 {
		return ProcessUnconfirmed, nil
	}
	fields, err := readProcStat(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if procEntryVanished(err) {
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

func GroupStopped(pid int) (bool, error) {
	if pid <= 0 {
		return false, syscall.EINVAL
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		fields, err := readProcStat(filepath.Join("/proc", entry.Name(), "stat"))
		if procEntryVanished(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if len(fields) <= 2 {
			return false, syscall.EINVAL
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		if group == pid && fields[0] != "Z" && fields[0] != "X" {
			return false, nil
		}
	}
	return true, nil
}

func procEntryVanished(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ESRCH)
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
