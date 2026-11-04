package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The debug log, which the user asked for on 2026-11-04 to chase the app not running (their rule is
// no logging otherwise: take it out once that's solved). q2-applemusic.log at the card's root, where
// File Browser shows it: a line each for a start (and what the run before it left in /tmp: a crash's
// message, the launcher's restarts and exit codes), the page's socket and the card's first read,
// sign-in changes, each download and failure, Download All's lists, slow page requests, memory and
// /tmp every minute, and why it stopped. Never the token. Each line is appended and synced, the file
// opened and closed around it, so it never keeps the card busy; past logMax it starts again, the last
// one kept as q2-applemusic.old.log. The launcher (contrib/shanlingq2/launcher.sh) writes its own
// lines there too.

// logPath is the log's file, "" for none (tests).
var logPath = ""

const logMax = 512 << 10

var (
	logMu   sync.Mutex
	started = time.Now()
)

func logf(format string, args ...any) {
	if logPath == "" {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05.000 ") + fmt.Sprintf(format, args...) + "\n"
	logMu.Lock()
	defer logMu.Unlock()
	if !cardReady() {
		return
	}
	if fi, err := os.Stat(logPath); err == nil && fi.Size()+int64(len(line)) > logMax {
		os.Rename(logPath, strings.TrimSuffix(logPath, ".log")+".old.log")
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	f.WriteString(line)
	f.Sync()
	f.Close()
}

// errFile is where the launcher puts the app's stderr (a crash's message) and its restarts.
const errFile = "/tmp/q2-applemusic.err"

// free is a file system's free space, in MB.
func free(path string) string {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return "?"
	}
	return fmt.Sprintf("%d MB", uint64(st.Bavail)*uint64(st.Bsize)>>20)
}

// memAvailable is the system's MemAvailable and MemFree, from /proc/meminfo.
func memAvailable() string {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return "?"
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); strings.HasPrefix(l, "MemAvailable:") || strings.HasPrefix(l, "MemFree:") {
			out = append(out, strings.Join(strings.Fields(l), " "))
		}
	}
	return strings.Join(out, ", ")
}

// logStart notes a start: what the run before it left, the card's and /tmp's free space, the queue.
func logStart(d *daemon) {
	if b, err := os.ReadFile(errFile); err == nil && len(b) > 0 {
		if len(b) > 8<<10 {
			b = b[len(b)-8<<10:]
		}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			logf("before this start: %s", l)
		}
		os.Truncate(errFile, 0)
	}
	d.dl.mu.Lock()
	queued, lists, failed := len(d.dl.waiting), len(d.dl.bulk.Lists), len(d.dl.failed)
	d.dl.mu.Unlock()
	logf("start %s (pid %d): card free %s, /tmp free %s, %s; %d songs queued, %d Download All lists, %d failed",
		version, os.Getpid(), free(filepath.Dir(d.dl.folder)), free("/tmp"), memAvailable(), queued, lists, failed)
}

// logMemory notes the memory, the system's and the app's, and /tmp.
func logMemory(d *daemon) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	d.dl.mu.Lock()
	queued, lists, have := len(d.dl.waiting), len(d.dl.bulk.Lists), len(d.dl.have)
	d.dl.mu.Unlock()
	logf("memory: app heap %d MB, app system %d MB, %d goroutines; %s; /tmp free %s; %d songs on the card, %d queued, %d lists to go",
		m.HeapAlloc>>20, m.Sys>>20, runtime.NumGoroutine(), memAvailable(), free("/tmp"), have, queued, lists)
}

var authNames = map[int]string{authUnknown: "not asked yet", authNoToken: "no sign-in on the card",
	authOK: "signed in", authFree: "no subscription", authExpired: "Apple refused the sign-in",
	authOffline: "Apple can't be reached"}
