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
)

type processStat struct {
	ppid    int
	pgrp    int
	session int
	ttyNr   int
	tpgid   int
}

type processCandidate struct {
	pid     int
	depth   int
	label   string
	stat    processStat
	hasStat bool
}

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

func normalizeCommandLabel(value string, stripExtension bool) string {
	label := normalizeProcessName(value)
	if stripExtension {
		if ext := filepath.Ext(label); ext != "" {
			label = strings.TrimSuffix(label, ext)
		}
	}
	return label
}

func commandLineLabel(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	executable := normalizeCommandLabel(fields[0], false)
	for _, field := range fields[1:] {
		field = strings.TrimSpace(field)
		if field == "" || strings.HasPrefix(field, "-") || !strings.Contains(field, "/") {
			continue
		}
		label := normalizeCommandLabel(field, true)
		if label != "" && label != executable {
			return label
		}
	}
	return executable
}

func processLabel(fields []string, comm string) string {
	executable := ""
	if len(fields) > 0 {
		executable = normalizeCommandLabel(fields[0], false)
	}
	commLabel := normalizeProcessName(comm)
	if commLabel != "" && (executable == "" || !commMatchesExecutable(commLabel, executable)) {
		return commLabel
	}
	if label := commandLineLabel(fields); label != "" {
		return label
	}
	return commLabel
}

func commMatchesExecutable(commLabel string, executable string) bool {
	if commLabel == executable {
		return true
	}
	return len(commLabel) > 0 && len(commLabel) <= 15 && strings.HasPrefix(executable, commLabel)
}

func processCommandName(pid int) string {
	fields := []string{}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err == nil && len(cmdline) > 0 {
		fields = strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	}
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return processLabel(fields, "")
	}
	return processLabel(fields, string(comm))
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
	stat, ok := readProcessStat(pid)
	if !ok {
		return 0
	}
	return stat.ppid
}

func readProcessStat(pid int) (processStat, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return processStat{}, false
	}
	raw := string(data)
	end := strings.LastIndex(raw, ")")
	if end < 0 || end+2 >= len(raw) {
		return processStat{}, false
	}
	fields := strings.Fields(raw[end+2:])
	if len(fields) < 6 {
		return processStat{}, false
	}
	parse := func(index int) (int, bool) {
		value, err := strconv.Atoi(fields[index])
		return value, err == nil
	}
	ppid, ok := parse(1)
	if !ok {
		return processStat{}, false
	}
	pgrp, ok := parse(2)
	if !ok {
		return processStat{}, false
	}
	session, ok := parse(3)
	if !ok {
		return processStat{}, false
	}
	ttyNr, ok := parse(4)
	if !ok {
		return processStat{}, false
	}
	tpgid, ok := parse(5)
	if !ok {
		return processStat{}, false
	}
	return processStat{
		ppid:    ppid,
		pgrp:    pgrp,
		session: session,
		ttyNr:   ttyNr,
		tpgid:   tpgid,
	}, true
}

func childProcessLabel(rootPID int) string {
	if rootPID <= 0 {
		return ""
	}

	var ppidChildren map[int][]int
	return selectChildProcessLabel(
		rootPID,
		func(pid int) []int {
			children, err := procChildrenFile(pid)
			if err == nil {
				return children
			}
			if ppidChildren == nil {
				ppidChildren = procChildrenByPPIDMap()
			}
			return ppidChildren[pid]
		},
		processCommandName,
		readProcessStat,
	)
}

func selectChildProcessLabel(rootPID int, children func(int) []int, name func(int) string, stat func(int) (processStat, bool)) string {
	if rootPID <= 0 {
		return ""
	}

	candidates := []processCandidate{}
	seen := map[int]bool{rootPID: true}
	queue := []processCandidate{{pid: rootPID}}
	for len(queue) > 0 && len(seen) < 96 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range children(current.pid) {
			if child <= 0 || seen[child] {
				continue
			}
			seen[child] = true
			label := name(child)
			childStat, hasStat := stat(child)
			candidate := processCandidate{
				pid:     child,
				depth:   current.depth + 1,
				label:   label,
				stat:    childStat,
				hasStat: hasStat,
			}
			if label != "" {
				candidates = append(candidates, candidate)
			}
			queue = append(queue, candidate)
		}
	}
	if label := foregroundProcessGroupLabel(candidates); label != "" {
		return label
	}
	if len(candidates) > 0 {
		return candidates[0].label
	}
	return ""
}

func foregroundProcessGroupLabel(candidates []processCandidate) string {
	// A terminal's foreground job is represented by tpgid. Prefer that process
	// group leader over deeper helper children.
	foregroundPgrp := 0
	for _, candidate := range candidates {
		if candidate.hasStat && candidate.stat.ttyNr != 0 && candidate.stat.tpgid > 0 {
			foregroundPgrp = candidate.stat.tpgid
			break
		}
	}
	if foregroundPgrp == 0 {
		return ""
	}

	var fallback *processCandidate
	for index := range candidates {
		candidate := &candidates[index]
		if !candidate.hasStat || candidate.stat.pgrp != foregroundPgrp {
			continue
		}
		if candidate.pid == foregroundPgrp {
			return candidate.label
		}
		if fallback == nil || candidate.depth < fallback.depth {
			fallback = candidate
		}
	}
	if fallback != nil {
		return fallback.label
	}
	return ""
}
