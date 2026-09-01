package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/mac-lucky/hassio-addons/ha_gitops_agent/internal/fsx"
)

// DefaultPath is where the agent keeps its run history, beside state.json
// under /data. Not on applier.Config: history governs nothing.
const DefaultPath = "/data/history.jsonl"

const (
	// maxBytes triggers a rewrite regardless of record count - keep counts
	// records, which only bounds the file while records stay small.
	maxBytes = 1 << 20

	// rotateTargetBytes is what a rewrite trims down TO; the gap below
	// maxBytes is headroom, or every append would rewrite the whole file.
	rotateTargetBytes = maxBytes * 3 / 4

	// rotateSlack is how many records past keep the file may grow before a
	// rewrite. Load still bounds reads to keep, which is the promise.
	rotateSlack = 50

	// maxLineBytes bounds one line; longer means a file no version of this
	// code wrote, so the scan stops there and returns what it has.
	maxLineBytes = 64 << 10

	// maxParseWarnings caps the per-read complaint, so a file full of
	// garbage produces a few log lines rather than one per line.
	maxParseWarnings = 3
)

// Store is the append-only run history at one path. Never fails to
// construct or to read; only Append reports an error (read-only /data).
type Store struct {
	path string
	keep int

	// mu guards the file and the three fields below. Callers already hold
	// internal/recon's opLock, but that is their invariant, not this type's.
	mu sync.Mutex

	// lines and bytes track the file so a plain append needs no stat or
	// re-scan. bytes is exact from Open; lines stays 0 until the first Load.
	lines int
	bytes int64

	// needsNewline records that the file does not end in one, so the next
	// Append closes off a torn line instead of splicing into it.
	needsNewline bool
}

// Open prepares the store at path, keeping the newest keep records (keep
// <= 0 disables retention). Never errors: unreadable yields an empty store.
func Open(path string, keep int) *Store {
	s := &Store{path: path, keep: keep}

	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		// Nothing here yet, or not a file. An add-on's first ever start is
		// the overwhelmingly common case, so this is not worth a warning.
		return s
	}

	s.bytes = info.Size()
	s.needsNewline = !endsWithNewline(path, info.Size())
	return s
}

// endsWithNewline reports whether the file's last byte is a newline, false
// exactly after a cut-short write. Empty or unreadable answers true.
func endsWithNewline(path string, size int64) bool {
	if size == 0 {
		return true
	}
	f, err := os.Open(path) // #nosec G304 -- caller-owned path under the add-on's own /data
	if err != nil {
		return true
	}
	defer func() { _ = f.Close() }()

	var b [1]byte
	if _, err := f.ReadAt(b[:], size-1); err != nil {
		return true
	}
	return b[0] == '\n'
}

// Load returns the retained records oldest-first, at most keep of them -
// the file may hold more. Unparseable lines are skipped, unknown ones kept.
func (s *Store) Load() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()

	data := s.readTail()
	recs, lines := parseRecords(data, s.path)

	// internal/recon Loads right after Open, so this is where the line
	// counter becomes exact.
	s.lines = lines

	if s.keep > 0 && len(recs) > s.keep {
		recs = recs[len(recs)-s.keep:]
	}
	return recs
}

// Append writes one record to the end of the file, rotating first if
// retention says so. One Write to O_APPEND bounds a crash to a torn line.
func (s *Store) Append(rec Record) error {
	// The last gate, not the only one: the caller applies Normalized when
	// the record is built, so the dashboard and the file agree.
	rec = rec.Normalized()

	marshalled, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureParent(); err != nil {
		return err
	}

	// One buffer, one Write - including the leading newline that closes off
	// a previous torn write.
	line := make([]byte, 0, len(marshalled)+2)
	if s.needsNewline {
		line = append(line, '\n')
	}
	line = append(line, marshalled...)
	line = append(line, '\n')

	// 0600: a record's Error field can quote whatever a failing apply or
	// Supervisor call echoed back, and that has included live values.
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- caller-owned path under the add-on's own /data
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		// How much of the buffer landed is unknown, so assume a torn line.
		s.needsNewline = true
		return err
	}
	if err := f.Close(); err != nil {
		s.needsNewline = true
		return err
	}

	s.needsNewline = false
	s.lines++
	s.bytes += int64(len(line))

	if s.keep > 0 && (s.lines > s.keep+rotateSlack || s.bytes > maxBytes) {
		s.rotate()
	}
	return nil
}

// rotate atomically rewrites the file to the newest records fitting both
// caps, copying bytes so newer fields survive. Caller holds s.mu.
func (s *Store) rotate() {
	data := s.readTail()
	// A crash-torn last line is not a record, so drop it rather than carry
	// it forward as the one line every future read has to skip.
	if end := bytes.LastIndexByte(data, '\n'); end >= 0 {
		data = data[:end+1]
	} else {
		data = nil
	}

	// Walk back one line at a time, newest first, until either cap is hit;
	// the result is a suffix of the file, so it needs no assembling.
	start, kept := len(data), 0
	for kept < s.keep && start > 0 {
		lineStart := bytes.LastIndexByte(data[:start-1], '\n') + 1
		// Always keep one, however fat: an empty file would make the next
		// append rotate again immediately.
		if kept > 0 && len(data)-lineStart > rotateTargetBytes {
			break
		}
		start, kept = lineStart, kept+1
	}
	buf := data[start:]

	// 0600 for Append's reason; atomic-with-fsync for state.json's.
	if err := fsx.WriteFileAtomic(s.path, buf, 0o600); err != nil {
		slog.Warn("history rotate failed", "path", s.path, "error", err)
		_ = os.Remove(s.path + ".tmp")
		return
	}

	s.lines = kept
	s.bytes = int64(len(buf))
	s.needsNewline = false
}

// readTail returns the file's contents, reading at most maxBytes from its
// end and starting at a line boundary. Caller holds s.mu.
func (s *Store) readTail() []byte {
	f, err := os.Open(s.path) // #nosec G304 -- caller-owned path under the add-on's own /data
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil
	}

	if info.Size() > maxBytes {
		slog.Warn("history file is larger than expected; only its newest records are being read",
			"path", s.path, "bytes", info.Size())
		if _, err := f.Seek(info.Size()-maxBytes, io.SeekStart); err != nil {
			return nil
		}
	}

	data, err := io.ReadAll(f)
	if err != nil {
		// Partial content is still worth more than none.
		slog.Warn("history read stopped early", "path", s.path, "error", err)
	}
	if info.Size() > maxBytes {
		// The seek almost certainly landed mid-record; that remnant is not
		// a record, so drop it rather than count it as a parse failure.
		if nl := bytes.IndexByte(data, '\n'); nl >= 0 {
			data = data[nl+1:]
		} else {
			data = nil
		}
	}
	return data
}

// parseRecords decodes newline-delimited records oldest-first, and counts
// every line including unparseable ones - that is the retention counter.
func parseRecords(data []byte, path string) (recs []Record, lines int) {
	var warnings int
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		lines++
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			if warnings < maxParseWarnings {
				slog.Warn("skipping unparseable history line", "path", path, "error", err)
				warnings++
			}
			continue
		}
		recs = append(recs, rec)
	}
	if err := scanner.Err(); err != nil {
		// An over-long line. A degraded history beats an empty one.
		slog.Warn("history scan stopped early", "path", path, "error", err, "records", len(recs))
	}
	return recs, lines
}

// ensureParent creates the file's parent directory, mirroring
// internal/applier.StateSave. Caller holds s.mu.
func (s *Store) ensureParent() error {
	parent := filepath.Dir(s.path)
	if parent == "." || parent == string(filepath.Separator) {
		return nil
	}
	return os.MkdirAll(parent, 0o755) // #nosec G301 -- 0755 deliberate, matching the rest of the add-on's /data layout
}
