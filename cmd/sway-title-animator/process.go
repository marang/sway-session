package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var (
	terminalNeedles = []string{
		"alacritty",
		"foot",
		"kitty",
		"wezterm",
		"terminal",
	}

	ignoredProcessNames = map[string]bool{
		"":          true,
		"alacritty": true,
		"bash":      true,
		"dash":      true,
		"env":       true,
		"fish":      true,
		"foot":      true,
		"kitty":     true,
		"login":     true,
		"sh":        true,
		"sudo":      true,
		"su":        true,
		"wezterm":   true,
		"zsh":       true,
	}
)

func isTerminalWindow(node *Node) bool {
	for _, value := range identifiers(node) {
		for _, needle := range terminalNeedles {
			if strings.Contains(value, needle) {
				return true
			}
		}
	}
	return false
}

func normalizeProcessName(value string) string {
	value = strings.TrimSpace(value)
	value = filepath.Base(value)
	value = strings.TrimSuffix(value, ".exe")
	return strings.ToLower(value)
}

func processCommandName(pid int) string {
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err == nil && len(cmdline) > 0 {
		fields := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if len(fields) > 0 {
			return normalizeProcessName(fields[0])
		}
	}
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return normalizeProcessName(string(comm))
}

func procChildren(pid int, ppidChildren map[int][]int) []int {
	children, err := procChildrenFile(pid)
	if err == nil && len(children) > 0 {
		return children
	}
	return ppidChildren[pid]
}

func procChildrenFile(pid int) ([]int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
	if err != nil {
		return nil, err
	}
	children := []int{}
	for _, field := range strings.Fields(string(data)) {
		childPID, err := strconv.Atoi(field)
		if err == nil && childPID > 0 {
			children = append(children, childPID)
		}
	}
	return children, nil
}

func procChildrenByPPIDMap() map[int][]int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		ppid := procPPID(pid)
		if ppid > 0 {
			children[ppid] = append(children[ppid], pid)
		}
	}
	for ppid := range children {
		sort.Ints(children[ppid])
	}
	return children
}

func procPPID(pid int) int {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	stat := string(data)
	end := strings.LastIndex(stat, ")")
	if end < 0 || end+2 >= len(stat) {
		return 0
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

func childProcessLabel(rootPID int) string {
	if rootPID <= 0 {
		return ""
	}

	ppidChildren := procChildrenByPPIDMap()
	return selectChildProcessLabel(
		rootPID,
		func(pid int) []int {
			return procChildren(pid, ppidChildren)
		},
		processCommandName,
	)
}

func selectChildProcessLabel(rootPID int, children func(int) []int, name func(int) string) string {
	if rootPID <= 0 {
		return ""
	}

	seen := map[int]bool{rootPID: true}
	queue := []int{rootPID}
	for len(queue) > 0 && len(seen) < 96 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range children(current) {
			if child <= 0 || seen[child] {
				continue
			}
			seen[child] = true
			label := name(child)
			if !ignoredProcessNames[label] {
				return label
			}
			queue = append(queue, child)
		}
	}
	return ""
}
