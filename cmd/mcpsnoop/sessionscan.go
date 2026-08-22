package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// serverIdentity is what makes two sessions the same server, gathered from
// whatever a log recorded about the run it opens.
//
// It is one type rather than one per command on purpose. inventory answers what
// has run here and stats answers how those servers behaved, and if they keyed
// differently the same machine would have two populations of servers with two
// sets of names, which is precisely the confusion the label already causes.
type serverIdentity struct {
	label     string
	transport string
	command   []string
	cwd       string
	endpoint  string
}

// key folds an identity into a comparable string.
//
// For stdio it is the recorded command and working directory, never the label,
// because labelFor derives the label from the command's last path element and
// two checkouts of one project, or two servers written in one language, collide
// there routinely. For HTTP it is the endpoint, since mcpsnoop launched nothing
// and the endpoint is what the transport specification makes the server's
// identity. A log carrying neither falls back to its label and transport, which
// is the most a trimmed or pre-meta log can honestly claim.
//
// A redacted command keys as itself. Two runs of one server, one scrubbed and
// one not, are two rows, and that is the honest answer: mcpsnoop cannot know
// what the placeholder replaced, and merging them would mean guessing that the
// hidden halves matched.
func (id serverIdentity) key() string {
	const sep = "\x00"
	switch {
	case len(id.command) > 0:
		return "stdio" + sep + id.cwd + sep + strings.Join(id.command, sep)
	case id.endpoint != "":
		return "http" + sep + id.endpoint
	default:
		return "label" + sep + id.transport + sep + id.label
	}
}

// sessionLog is one candidate log with the modification time the selection rule
// orders by.
type sessionLog struct {
	path    string
	modTime time.Time
}

// sessionLogs lists the session logs a walk should read, newest first, bounded.
//
// Recency is modification time rather than name, matching hub.backfill and the
// exporter's newest-session lookup. A log is named <label>-<pid>-<nonce>.jsonl,
// so ordering by name orders by label and would keep whichever labels sort last
// rather than whichever sessions ran last.
//
// total is how many logs the directory held before the limit, so a caller can
// say it read a window rather than letting a bounded answer pass for a complete
// one. Zero-byte logs are counted in total and never returned: that is what a run
// whose exec failed leaves, and what an HTTP proxy nobody called leaves on
// purpose. Anything that is not a regular file is skipped outright, since os.Open
// on a fifo blocks until a writer appears and would hang the walk.
func sessionLogs(dir string, since time.Time, limit int) (logs []sessionLog, counts logCounts, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, logCounts{}, err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			counts.skipped++
			continue
		}
		counts.total++
		if info.Size() == 0 {
			counts.empty++
			continue
		}
		if !since.IsZero() && info.ModTime().Before(since) {
			continue
		}
		logs = append(logs, sessionLog{path: path, modTime: info.ModTime()})
	}
	slices.SortFunc(logs, func(a, b sessionLog) int {
		if c := b.modTime.Compare(a.modTime); c != 0 {
			return c
		}
		// Modification times collide readily on a fast machine, so the tie is broken
		// on the path. Without it the window a limit selects varies between runs over
		// an unchanged directory.
		return strings.Compare(a.path, b.path)
	})
	if limit > 0 && len(logs) > limit {
		logs = logs[:limit]
	}
	return logs, counts, nil
}

// logCounts is what a walk stepped over, so two commands reading one directory
// describe it the same way. total counts the regular .jsonl files it found,
// empty the zero-byte ones among them, and skipped the entries that are not
// regular files at all, which is a fifo, a socket, or a directory named like a
// log.
type logCounts struct {
	total   int
	empty   int
	skipped int
}

// maxAgeDays is the largest whole number of days a time.Duration can hold, which
// is math.MaxInt64 nanoseconds divided by a day.
const maxAgeDays = int(math.MaxInt64 / (24 * int64(time.Hour)))

// parseAge turns a --since or --older-than value into a duration. time.ParseDuration
// rejects a day suffix, so a whole number of days is parsed here on top of the Go
// duration forms it does accept.
//
// A zero or negative age is refused rather than treated as "everything". For
// prune that keeps deletion out of reach by accident, and everywhere else it
// keeps a flag that reads as a window from silently meaning its opposite.
func parseAge(value, flag string) (time.Duration, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, fmt.Errorf("%s is required, e.g. 30d or 72h", flag)
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid %s %q, want a whole number of days like 30d, or a Go duration like 72h", flag, s)
		}
		if n <= 0 {
			return 0, fmt.Errorf("%s must be greater than zero", flag)
		}
		// A duration is an int64 of nanoseconds, so 106752 days multiplies past the
		// end of it and lands on a negative one. That is not an overflow anybody
		// would notice: prune would compute a cutoff in the future and delete every
		// log in the directory, which is the one thing --older-than exists to keep
		// out of reach.
		if n > maxAgeDays {
			return 0, fmt.Errorf("%s %q is beyond the largest age this can express, %d days", flag, s, maxAgeDays)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q, want a day count like 30d, or a Go duration like 72h", flag, s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", flag)
	}
	return d, nil
}
