package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultTail is how much of the log to show without -n. A screenful,
	// roughly: enough to see what a tunnel has been doing, not so much that the
	// interesting part scrolls away.
	defaultTail = 50
	// maxTailBytes bounds the read for the tail, so `hop log` on a tunnel that
	// has been up for a week doesn't pull the whole file into memory.
	maxTailBytes = 1 << 20
	// followPoll is how often -f checks for new output. The log is written by
	// another process, so there is nothing to wait on but the file itself.
	followPoll = 250 * time.Millisecond
)

// runLog prints a detached agent's output.
//
// Only detached agents have a log to print: a foreground one writes to the
// terminal it was started in, and inventing a second copy on disk would mean
// deciding whether to duplicate or redirect output the user is already reading.
func runLog(args []string) {
	fs := flag.NewFlagSet("hop log", flag.ExitOnError)
	var (
		lines  = fs.Int("n", defaultTail, "how many lines to show")
		follow bool
	)
	fs.BoolVar(&follow, "f", false, "keep printing as the agent writes")
	fs.BoolVar(&follow, "follow", false, "keep printing as the agent writes")
	noColour := fs.Bool("no-color", false, "disable colour")
	fs.Usage = usage
	names := parseFlags(fs, args)

	// The file itself is plain: the agent wrote it to a file, and writing
	// escape codes into one would corrupt it for grep and every other reader.
	// Colour belongs here instead, where the output stream is known.
	initColourOn(os.Stdout, *noColour)

	if len(names) != 1 {
		fmt.Fprintln(os.Stderr, "hop: which tunnel? e.g. hop log myapp")
		listLogs()
		os.Exit(2)
	}
	path := resolveLog(names[0])

	f, err := os.Open(path)
	if err != nil {
		fatal("%v", err)
	}
	defer f.Close()

	end, err := printTail(f, *lines)
	if err != nil {
		fatal("%v", err)
	}
	if !follow {
		return
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		fatal("%v", err)
	}
	followFile(f)
}

// resolveLog turns a name, PID or local port into a log file, or explains why
// there isn't one and exits.
func resolveLog(want string) string {
	live, _ := liveStates()

	if matches, ok := findState(live, want); ok {
		if len(matches) > 1 {
			ambiguousRef(want, matches)
			os.Exit(1)
		}
		s := matches[0]
		if s.Log == "" {
			// Running, but in a terminal. Saying so is more useful than "no log
			// file", which reads as something being broken.
			fmt.Fprintf(os.Stderr, "hop: %q is running in the foreground (pid %d) — its output is in the terminal that started it.\n",
				want, s.PID)
			fmt.Fprintf(os.Stderr, "\n  Only detached tunnels (started with -d) keep a log.\n")
			os.Exit(1)
		}
		return s.Log
	}

	// Not running. A PID may still name the log of an agent that has since
	// exited, which is exactly when you want to read one.
	if pid, err := strconv.Atoi(want); err == nil {
		if path, err := logPathFor(pid); err == nil {
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
	}

	fmt.Fprintf(os.Stderr, "hop: no tunnel named %q is running on this machine\n", want)
	listLogs()
	os.Exit(1)
	return ""
}

// printTail writes the last n lines of f and returns the offset it read up to,
// so -f can carry on from there without re-printing anything.
func printTail(f *os.File, n int) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()

	start := int64(0)
	if size > maxTailBytes {
		start = size - maxTailBytes
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return 0, err
	}
	// A window into the middle of the file almost certainly begins mid-line;
	// drop that fragment rather than printing half a line.
	if start > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}

	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		fmt.Println(render(string(line)))
	}
	return size, nil
}

// followFile prints what the agent writes from here on, until interrupted.
//
// Read a line at a time rather than copied wholesale, since each one is
// recoloured on the way out. A write caught mid-line is held until its newline
// arrives, so a partial line is never printed and then continued.
func followFile(f *os.File) {
	r := bufio.NewReader(f)
	var pending []byte
	for {
		chunk, err := r.ReadBytes('\n')
		pending = append(pending, chunk...)
		if n := len(pending); n > 0 && pending[n-1] == '\n' {
			fmt.Println(render(string(pending[:n-1])))
			pending = nil
		}
		switch {
		case err == io.EOF:
			time.Sleep(followPoll)
		case err != nil:
			fatal("%v", err)
		}
	}
}

// requestLine matches the fixed layout of a logged request, capturing the two
// fields worth colouring: the method and the status. Everything else — the
// padding included — is passed through untouched, so alignment survives.
var requestLine = regexp.MustCompile(`^(\s+)([A-Z?]{1,9})(\s+)(\d{3}|---)(\s)`)

// render re-applies the colour the agent could not, because it was writing to a
// file. Lines that aren't request lines — the URL banner, reconnect notices —
// are left exactly as they are.
func render(line string) string {
	if !colour {
		return line
	}
	m := requestLine.FindStringSubmatchIndex(line)
	if m == nil {
		return line
	}
	// Pairs of indices, two per group: 2-3 is the leading space, 4-5 the
	// method, 6-7 the gap between them, 8-9 the status.
	method := line[m[4]:m[5]]
	status := line[m[8]:m[9]]
	code, _ := strconv.Atoi(status) // "---" yields 0, which statusColour dims
	return line[:m[4]] +
		paint(method, methodColour(method)) +
		line[m[5]:m[8]] +
		paint(status, statusColour(code)) +
		line[m[9]:]
}

// listLogs shows what there is to read: the tunnels running now, and the logs
// left behind by ones that have finished.
func listLogs() {
	live, _ := liveStates()
	var running []string
	for _, s := range live {
		if s.Subdomain != "" && s.Log != "" {
			running = append(running, s.Subdomain)
		}
	}
	if len(running) > 0 {
		fmt.Fprintf(os.Stderr, "\n  Detached and running: %s\n", strings.Join(running, ", "))
	}

	past := pastLogs(live)
	if len(past) > 0 {
		fmt.Fprintf(os.Stderr, "\n  Logs from finished agents (name them by pid):\n")
		for _, p := range past {
			fmt.Fprintf(os.Stderr, "    %-8d %s\n", p.pid, p.when.Format("Jan 2 15:04"))
		}
	}
	if len(running) == 0 && len(past) == 0 {
		fmt.Fprintln(os.Stderr, "\n  No logs yet — only tunnels started with -d keep one.")
	}
}

type pastLog struct {
	pid  int
	when time.Time
}

// pastLogs lists the most recent logs whose agent is gone, newest first.
func pastLogs(live []State) []pastLog {
	dir, err := subDir("log")
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	alive := make(map[int]bool, len(live))
	for _, s := range live {
		alive[s.PID] = true
	}

	var out []pastLog
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".log"))
		if err != nil || alive[pid] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, pastLog{pid: pid, when: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].when.After(out[j].when) })
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}
