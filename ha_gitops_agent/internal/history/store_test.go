package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// rec builds a record distinguishable from its neighbours, so a retention
// test can assert WHICH records survived rather than only how many.
func rec(n int) Record {
	return Record{
		Kind:       KindReconcile,
		StartedUTC: "2026-08-05T12:00:00Z",
		DurationMS: int64(n),
		SHA:        strings.Repeat("a", 39) + string(rune('0'+n%10)),
		Outcome:    OutcomeInSync,
	}
}

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "history.jsonl")
}

func mustAppend(t *testing.T, s *Store, r Record) {
	t.Helper()
	if err := s.Append(r); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func onDiskLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Count(string(data), "\n")
}

func TestAppendThenLoadRoundTripsEveryField(t *testing.T) {
	path := tempPath(t)
	s := Open(path, 10)

	want := Record{
		Kind:       KindApply,
		StartedUTC: "2026-08-05T12:31:04Z",
		DurationMS: 4200,
		SHA:        "a3f9c2140e5b6d7889aabbccddeeff0011223344",
		Outcome:    OutcomePartial,
		Files:      6,
		RegOps:     3,
		Error:      "integrations: pushward create failed",
		StashDir:   "/data/backup/20260805T123100Z",
	}
	mustAppend(t, s, want)

	got := Open(path, 10).Load()
	if len(got) != 1 {
		t.Fatalf("Load returned %d records, want 1", len(got))
	}
	if got[0] != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got[0], want)
	}
}

func TestLoadReturnsOldestFirst(t *testing.T) {
	path := tempPath(t)
	s := Open(path, 10)
	for i := 1; i <= 3; i++ {
		mustAppend(t, s, rec(i))
	}

	got := Open(path, 10).Load()
	if len(got) != 3 {
		t.Fatalf("Load returned %d records, want 3", len(got))
	}
	for i, r := range got {
		if want := int64(i + 1); r.DurationMS != want {
			t.Errorf("record %d has DurationMS %d, want %d (not oldest-first)", i, r.DurationMS, want)
		}
	}
}

// A partly-corrupt file must cost the rows that are corrupt and nothing
// else - the same contract applier.StateLoad keeps for state.json.
func TestLoadSkipsUnparseableLinesAndKeepsTheRest(t *testing.T) {
	path := tempPath(t)
	good1, _ := json.Marshal(rec(1))
	good2, _ := json.Marshal(rec(2))
	body := string(good1) + "\n{garbage\n\n" + string(good2) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Open(path, 10).Load()
	if len(got) != 2 {
		t.Fatalf("Load returned %d records, want 2 (the two parseable ones)", len(got))
	}
	if got[0].DurationMS != 1 || got[1].DurationMS != 2 {
		t.Errorf("wrong records survived: %+v", got)
	}
}

func TestLoadOnAMissingFileReturnsNothing(t *testing.T) {
	s := Open(tempPath(t), 10)
	if got := s.Load(); len(got) != 0 {
		t.Errorf("Load on a missing file returned %d records, want 0", len(got))
	}
}

func TestOpenOnADirectoryDoesNotPanicAndLoadsNothing(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir, 10)
	if got := s.Load(); len(got) != 0 {
		t.Errorf("Load on a directory returned %d records, want 0", len(got))
	}
}

// keep <= 0 turns retention off, so Load stops bounding too - there is no
// promise left for it to enforce.
func TestLoadReturnsEverythingWhenRetentionIsDisabled(t *testing.T) {
	path := tempPath(t)
	s := Open(path, 0)
	for i := 1; i <= rotateSlack+5; i++ {
		mustAppend(t, s, rec(i))
	}

	if got := s.Load(); len(got) != rotateSlack+5 {
		t.Errorf("Load returned %d records, want all %d", len(got), rotateSlack+5)
	}
}

func TestAppendCreatesTheFileAndItsParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "history.jsonl")
	s := Open(path, 10)
	mustAppend(t, s, rec(1))

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file was not created: %v", err)
	}
	if got := s.Load(); len(got) != 1 {
		t.Errorf("Load returned %d records, want 1", len(got))
	}
}

// The retention boundary: past keep+rotateSlack the file is rewritten
// down to exactly keep, and it is the NEWEST keep that survive.
func TestRotationKeepsTheNewestKeepRecords(t *testing.T) {
	path := tempPath(t)
	keep := 3
	s := Open(path, keep)

	total := keep + rotateSlack + 1
	for i := 1; i <= total; i++ {
		mustAppend(t, s, rec(i))
	}

	if got := onDiskLines(t, path); got != keep {
		t.Fatalf("file holds %d lines after rotation, want %d", got, keep)
	}
	got := Open(path, keep).Load()
	if len(got) != keep {
		t.Fatalf("Load returned %d records, want %d", len(got), keep)
	}
	for i, r := range got {
		if want := int64(total - keep + 1 + i); r.DurationMS != want {
			t.Errorf("record %d is #%d, want #%d (the newest %d should survive)", i, r.DurationMS, want, keep)
		}
	}
}

// The slack is what stops a rewrite happening on every single append.
func TestNoRotationUntilTheSlackIsExhausted(t *testing.T) {
	path := tempPath(t)
	keep := 3
	s := Open(path, keep)

	for i := 1; i <= keep+rotateSlack; i++ {
		mustAppend(t, s, rec(i))
	}
	if got := onDiskLines(t, path); got != keep+rotateSlack {
		t.Fatalf("file holds %d lines at the slack boundary, want %d (rotated too early)", got, keep+rotateSlack)
	}

	mustAppend(t, s, rec(keep+rotateSlack+1))
	if got := onDiskLines(t, path); got != keep {
		t.Fatalf("file holds %d lines one past the boundary, want %d", got, keep)
	}
}

// Load bounds what it returns even while the file is inside the slack, so
// keep stays the promise about what is READABLE.
func TestLoadHonoursKeepWhileTheFileIsInsideTheSlack(t *testing.T) {
	path := tempPath(t)
	keep := 3
	s := Open(path, keep)
	for i := 1; i <= keep+rotateSlack; i++ {
		mustAppend(t, s, rec(i))
	}

	if got := s.Load(); len(got) != keep {
		t.Errorf("Load(%d) returned %d records, want %d", keep, len(got), keep)
	}
}

// The byte ceiling is the guard that makes the count cap trustworthy: a
// run of records too fat for keep to bound must still rotate.
func TestByteCeilingRotatesBeforeTheCountCapWouldHave(t *testing.T) {
	path := tempPath(t)
	keep := 10_000 // far beyond anything the byte ceiling will allow
	s := Open(path, keep)

	fat := rec(1)
	fat.Error = strings.Repeat("x", ErrorMaxLen)

	var (
		appends             = (maxBytes/ErrorMaxLen)*2 + 10
		rotations           int
		sizeAfterLastRotate int64
	)
	for i := 0; i < appends; i++ {
		before := s.bytes
		mustAppend(t, s, fat)
		if s.bytes < before {
			rotations++
			sizeAfterLastRotate = s.bytes
		}
		if s.bytes > maxBytes {
			t.Fatalf("append %d left the file at %d bytes, past the %d ceiling", i, s.bytes, maxBytes)
		}
	}

	if rotations == 0 {
		t.Fatalf("the byte ceiling never fired across %d fat appends; keep alone was never going to bound this", appends)
	}
	// A rewrite must leave HEADROOM, not stop at the trigger, or the next
	// append trips it again - the pathology rotateTargetBytes prevents.
	if sizeAfterLastRotate > rotateTargetBytes {
		t.Errorf("a rewrite left the file at %d bytes, want at most %d", sizeAfterLastRotate, rotateTargetBytes)
	}
	if sizeAfterLastRotate == 0 {
		t.Error("a rewrite emptied the file, want the newest records that fit")
	}
	// What the headroom buys: hundreds of appends per rewrite. The bound is
	// loose on purpose - it only has to separate amortized from thrashing.
	const minAppendsPerRewrite = 100
	if got := appends / rotations; got < minAppendsPerRewrite {
		t.Errorf("%d rewrites across %d appends (%d apiece); want at least %d apiece, or the ceiling is thrashing",
			rotations, appends, got, minAppendsPerRewrite)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxBytes {
		t.Errorf("file is %d bytes on disk, want at most %d", info.Size(), maxBytes)
	}
}

func TestErrorIsTruncatedBeforeItReachesDisk(t *testing.T) {
	path := tempPath(t)
	s := Open(path, 10)

	r := rec(1)
	r.Error = strings.Repeat("e", 10_000)
	mustAppend(t, s, r)

	got := Open(path, 10).Load()
	if len(got) != 1 {
		t.Fatalf("Load returned %d records, want 1", len(got))
	}
	if len(got[0].Error) > ErrorMaxLen {
		t.Errorf("stored error is %d chars, want at most %d", len(got[0].Error), ErrorMaxLen)
	}
	if !strings.HasSuffix(got[0].Error, "(truncated)") {
		t.Errorf("truncated error does not say so: %q", got[0].Error)
	}
}

func TestTruncationCutsOnARuneBoundary(t *testing.T) {
	r := rec(1)
	// Built from code points so this file stays ASCII: U+00E4 is a two-byte
	// rune to cut inside, U+FFFD the replacement char a bad cut leaves.
	const (
		twoByteRune     = rune(0x00E4)
		replacementRune = rune(0xFFFD)
	)
	r.Error = strings.Repeat("z", ErrorMaxLen-5) + strings.Repeat(string(twoByteRune), 100)
	got := truncateError(r.Error)

	if strings.ContainsRune(got, replacementRune) {
		t.Errorf("truncation split a rune: %q", got)
	}
	if len(got) > ErrorMaxLen {
		t.Errorf("truncated to %d chars, want at most %d", len(got), ErrorMaxLen)
	}
}

func TestAShortErrorIsLeftAlone(t *testing.T) {
	const msg = "check_config failed"
	if got := truncateError(msg); got != msg {
		t.Errorf("truncateError(%q) = %q, want it unchanged", msg, got)
	}
}

// A file grown past maxBytes by something outside the agent must degrade
// into a bounded tail read plus a rewrite, not an unbounded load.
func TestAnAbsurdlyLargeFileIsReadFromItsTailAndThenTrimmed(t *testing.T) {
	path := tempPath(t)

	// Pad past maxBytes with parseable records, then finish with a marker
	// whose SHA identifies it as the newest.
	pad := rec(1)
	pad.Error = strings.Repeat("p", ErrorMaxLen)
	line, _ := json.Marshal(pad)
	perLine := len(line) + 1

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for written := 0; written < maxBytes+2*perLine; written += perLine {
		if _, err := f.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			t.Fatal(err)
		}
	}
	marker := rec(7)
	marker.SHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	markerLine, _ := json.Marshal(marker)
	if _, err := f.Write(append(markerLine, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	s := Open(path, 5)
	got := s.Load()
	if len(got) == 0 {
		t.Fatal("Load returned nothing from an oversized file, want the tail")
	}
	if got[len(got)-1].SHA != marker.SHA {
		t.Errorf("newest record is %q, want the marker %q", got[len(got)-1].SHA, marker.SHA)
	}

	mustAppend(t, s, rec(8))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxBytes {
		t.Errorf("file is still %d bytes after an append, want it trimmed to at most %d", info.Size(), maxBytes)
	}
}

// The crash case Append's single-buffer write is designed around: a torn
// final line costs that row and nothing more, and the file stays writable.
func TestATornLastLineIsSkippedAndTheNextAppendStillWorks(t *testing.T) {
	path := tempPath(t)
	s := Open(path, 10)
	mustAppend(t, s, rec(1))
	mustAppend(t, s, rec(2))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	torn := string(data[:len(data)-12]) // cut into the final record
	if err := os.WriteFile(path, []byte(torn), 0o644); err != nil {
		t.Fatal(err)
	}

	reopened := Open(path, 10)
	got := reopened.Load()
	if len(got) != 1 || got[0].DurationMS != 1 {
		t.Fatalf("after a torn write Load returned %+v, want only the first record", got)
	}

	mustAppend(t, reopened, rec(3))
	got = Open(path, 10).Load()
	if len(got) == 0 || got[len(got)-1].DurationMS != 3 {
		t.Errorf("append after a torn write did not land: %+v", got)
	}
	// The new record must be a line of its own. Splicing it onto the
	// torn one would lose both, which is the whole failure this guards.
	if len(got) != 2 {
		t.Errorf("after a torn write and one append Load returned %d records, want 2", len(got))
	}
}

// Same splice, reached the other way: the process that wrote the torn
// line is still running, so the fix cannot depend on a reopen.
func TestAnAppendAfterATornWriteInTheSameProcessDoesNotSplice(t *testing.T) {
	path := tempPath(t)
	s := Open(path, 10)
	mustAppend(t, s, rec(1))

	// Truncate behind the store's back, the way a crash would have.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-8], 0o644); err != nil {
		t.Fatal(err)
	}
	s.needsNewline = true

	mustAppend(t, s, rec(2))
	got := Open(path, 10).Load()
	if len(got) != 1 || got[0].DurationMS != 2 {
		t.Errorf("Load returned %+v, want only the record appended after the tear", got)
	}
}

// Forward compatibility, the read half of the add-only rule: a line
// written by a newer binary must come back usable, not be discarded.
func TestUnknownFieldsFromANewerVersionAreIgnored(t *testing.T) {
	path := tempPath(t)
	body := `{"kind":"apply","started_utc":"2026-08-05T12:00:00Z","outcome":"ok","files":2,"trigger":"webhook","dry_run":true}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Open(path, 10).Load()
	if len(got) != 1 {
		t.Fatalf("Load returned %d records, want 1", len(got))
	}
	if got[0].Kind != KindApply || got[0].Files != 2 {
		t.Errorf("known fields were not decoded: %+v", got[0])
	}
}

func TestAnUnknownKindOrOutcomeIsKeptNotDropped(t *testing.T) {
	path := tempPath(t)
	body := `{"kind":"prune","started_utc":"2026-08-05T12:00:00Z","outcome":"deferred"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Open(path, 10).Load()
	if len(got) != 1 {
		t.Fatalf("Load returned %d records, want 1 (an unrecognized kind must survive)", len(got))
	}
	if got[0].Kind != "prune" || got[0].Outcome != "deferred" {
		t.Errorf("unrecognized values were rewritten: %+v", got[0])
	}
}

// The store carries its own mutex rather than relying on the caller's lock
// discipline. Runs under -race, as the justfile's test recipe does.
func TestAppendIsSafeUnderConcurrentCallers(t *testing.T) {
	path := tempPath(t)
	s := Open(path, 100)

	const writers, each = 8, 25
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := s.Append(rec(i)); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := len(s.Load()); got != 100 {
		t.Errorf("Load returned %d records, want the retained 100", got)
	}
}

func TestKeepOfZeroDisablesRotation(t *testing.T) {
	path := tempPath(t)
	s := Open(path, 0)
	for i := 1; i <= rotateSlack+5; i++ {
		mustAppend(t, s, rec(i))
	}
	if got := onDiskLines(t, path); got != rotateSlack+5 {
		t.Errorf("file holds %d lines, want %d (keep=0 must not rotate)", got, rotateSlack+5)
	}
}

func TestRecordShortSHA(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"a3f9c21": "a3f9c21",
		"a3f9c":   "a3f9c",
		"a3f9c2140e5b6d7889aabbccddeeff0011223344": "a3f9c21",
	}
	for in, want := range cases {
		if got := (Record{SHA: in}).ShortSHA(); got != want {
			t.Errorf("ShortSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecordDuration(t *testing.T) {
	if got := (Record{DurationMS: 4200}).Duration(); got != 4200*time.Millisecond {
		t.Errorf("Duration() = %v, want 4.2s", got)
	}
	if got := (Record{}).Duration(); got != 0 {
		t.Errorf("Duration() on a zero record = %v, want 0", got)
	}
}

// Rotation copies bytes rather than re-marshalling, so fields a newer
// binary wrote survive a trim by an older one.
func TestRotationPreservesFieldsThisBinaryDoesNotKnow(t *testing.T) {
	path := tempPath(t)
	keep := 2

	var body strings.Builder
	for i := 1; i <= keep+rotateSlack+1; i++ {
		body.WriteString(`{"kind":"apply","started_utc":"2026-08-05T12:00:00Z","outcome":"ok","files":`)
		body.WriteString(strings.Repeat("0", 0))
		body.WriteString("1")
		body.WriteString(`,"trigger":"webhook","dry_run":true}`)
		body.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	s := Open(path, keep)
	s.lines = keep + rotateSlack + 1 // as a Load would have set it
	mustAppend(t, s, rec(1))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"trigger":"webhook"`) {
		t.Error("rotation dropped a field this binary does not know; it must copy bytes, not re-marshal")
	}
	if !strings.Contains(string(data), `"dry_run":true`) {
		t.Error("rotation dropped a second unknown field")
	}
}

// The same rule applied to a line that does not parse at all: the file is
// not this code's to edit, only to bound.
func TestRotationPreservesAnUnparseableLine(t *testing.T) {
	path := tempPath(t)
	keep := 2

	var body strings.Builder
	body.WriteString("{this-was-here-before\n")
	for i := 1; i <= keep+rotateSlack; i++ {
		line, _ := json.Marshal(rec(i))
		body.Write(line)
		body.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	s := Open(path, keep+rotateSlack+5) // keep everything, force only the byte path off
	s.lines = keep + rotateSlack + 1
	mustAppend(t, s, rec(99))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "{this-was-here-before") {
		t.Error("rotation collected a line it could not parse, want it left alone")
	}
}

// Normalized is what makes the in-memory copy and the on-disk copy agree.
func TestNormalizedBoundsTheError(t *testing.T) {
	r := rec(1)
	r.Error = strings.Repeat("e", 10_000)

	got := r.Normalized()

	if len(got.Error) > ErrorMaxLen {
		t.Errorf("Normalized left a %d-char error, want at most %d", len(got.Error), ErrorMaxLen)
	}
	if r.Error == got.Error {
		t.Error("Normalized returned the record unchanged")
	}
}

func TestRecordCounts(t *testing.T) {
	cases := []struct {
		files, regOps int
		want          string
	}{
		{6, 3, "6 file(s), 3 reg op(s)"},
		{6, 0, "6 file(s)"},
		{0, 3, "3 reg op(s)"},
		{0, 0, "-"},
	}
	for _, tc := range cases {
		got := Record{Files: tc.files, RegOps: tc.regOps}.Counts()
		if got != tc.want {
			t.Errorf("Counts(files=%d, regOps=%d) = %q, want %q", tc.files, tc.regOps, got, tc.want)
		}
	}
}
