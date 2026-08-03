package jobs

import (
	"bufio"
	"os"
)

// lineFile is a simple append-only, line-buffered file writer used to persist
// step logs to disk so they survive process restarts and can be replayed to
// SSE subscribers that connect after the fact (e.g. after a browser refresh).
type lineFile struct {
	f *os.File
	w *bufio.Writer
}

func openLineFile(path string) (*lineFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &lineFile{f: f, w: bufio.NewWriter(f)}, nil
}

func (l *lineFile) WriteLine(s string) {
	l.w.WriteString(s)
	l.w.WriteByte('\n')
	l.w.Flush() // small perf cost, but a build log you can't tail live is useless
}

func (l *lineFile) Close() {
	l.w.Flush()
	l.f.Close()
}

// ReadAllLines reads a persisted step log from disk (used to replay history
// to an SSE subscriber that connects mid-job or after completion).
func ReadAllLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}
