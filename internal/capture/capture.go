// Package capture implements an append-only on-disk recorder and replay reader
// for live crypto market data streams. The format uses a gzip-compressed gob
// stream with per-record gap tracking to maintain tape fidelity.
//
// The writer is non-blocking and designed for single-writer loops that must
// never be stalled by I/O. Gap records are localized in the stream so
// downstream consumers can measure execution costs honestly: a tape with a
// hole in it prices orders against a book that skipped updates, and the only
// thing worse than losing events is not knowing where you lost them.
//
// Files rotate hourly and are capped by size. The reader expects contiguous
// sequences and tolerates a truncated final file (the normal result of a
// crash or a kill) while rejecting truncation anywhere else.
package capture

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

const (
	// FormatVersion identifies the on-disk format version for future compatibility.
	FormatVersion = 1
)

// Header is the first gob value in every file, defining the stream metadata.
type Header struct {
	Format  int
	RunID   string
	Seq     int
	Started time.Time
	Venues  []string
	Symbols map[string]string
	Depth   int
}

// Record is a single data point in the stream. Gap >0 means events were
// dropped immediately before this record and Event is zero-valued.
type Record struct {
	Wall  time.Time
	Gap   uint64
	Event model.Event
}

// Writer is an append-only recorder that writes model.Event values to disk.
type Writer struct {
	cfg     Config
	drainCh chan model.Event
	dropCnt atomic.Uint64
	wg      sync.WaitGroup
	closed  atomic.Bool

	// File state below is owned exclusively by the drain goroutine. mu is
	// held only for the brief windows where Stats (called from another
	// goroutine) reads these fields — not around any I/O.
	mu       sync.Mutex
	enc      *gob.Encoder
	gzw      *gzip.Writer
	cw       *countingWriter
	file     *os.File
	fileSeq  int
	fileHour time.Time
	curName  string
	records  uint64
	err      error
}

// countingWriter tallies the bytes actually reaching the file so rotation is
// driven by real on-disk size. Counting encoded records and assuming a fixed
// size per record does not measure anything: record sizes vary by an order of
// magnitude between a one-level update and a thousand-level snapshot.
//
// The count lags by whatever gzip is still holding in its block buffer, so
// MaxBytes is an approximate ceiling that trips slightly late. At the 512 MB
// default that lag is a rounding error; it only shows up with a MaxBytes small
// enough to sit inside a single compression block.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Config holds configuration for a new Writer.
type Config struct {
	Dir      string
	RunID    string
	Venues   []string
	Symbols  map[string]string
	Depth    int
	BufSize  int
	MaxBytes int64
	Now      func() time.Time
}

// Stats returns current writer statistics.
type Stats struct {
	Written     uint64
	Dropped     uint64
	Files       int
	CurrentFile string
}

// NewWriter creates a new Writer that records to files in cfg.Dir.
func NewWriter(cfg Config) (*Writer, error) {
	if cfg.Dir == "" {
		return nil, errors.New("capture: Dir must not be empty")
	}
	if cfg.BufSize <= 0 {
		cfg.BufSize = 65536
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 512 << 20
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RunID == "" {
		r := make([]byte, 4)
		if _, err := rand.Read(r); err != nil {
			return nil, fmt.Errorf("capture: generate run ID: %w", err)
		}
		cfg.RunID = cfg.Now().Format("20060102-150405") + "-" + hex.EncodeToString(r)
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("capture: mkdir: %w", err)
	}

	w := &Writer{
		cfg:     cfg,
		drainCh: make(chan model.Event, cfg.BufSize),
	}
	if err := w.openFile(); err != nil {
		return nil, err
	}

	w.wg.Add(1)
	go w.drain()

	return w, nil
}

// Offer enqueues an event for recording. It never blocks and is safe to call
// concurrently with Stats and Close. If the buffer is full, the event is dropped
// and a gap record will be emitted when the buffer drains.
func (w *Writer) Offer(ev model.Event) {
	if w.closed.Load() {
		return
	}
	select {
	case w.drainCh <- ev:
	default:
		// Non-blocking send failed; increment pending drop counter.
		// The drain goroutine will localize this drop as a gap record.
		w.dropCnt.Add(1)
	}
}

// Close stops the writer, drains remaining records, and closes the file.
// It is idempotent and safe to call multiple times.
func (w *Writer) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(w.drainCh)
	w.wg.Wait()
	return w.err
}

// Stats returns current writer statistics.
func (w *Writer) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Stats{
		Written:     w.records, // RECORDS encoded, not bytes
		Dropped:     w.dropCnt.Load(),
		Files:       w.fileSeq + 1,
		CurrentFile: w.curName,
	}
}

// openFile opens the file for the CURRENT w.fileSeq and writes its header.
// Called from NewWriter and from rotate, both on paths where no lock is held —
// it takes mu itself, only to publish the new state to Stats.
func (w *Writer) openFile() error {
	now := w.cfg.Now()
	fname := fmt.Sprintf("cap-%s-%06d.gob.gz", w.cfg.RunID, w.fileSeq)
	path := filepath.Join(w.cfg.Dir, fname)

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("capture: create %s: %w", path, err)
	}
	cw := &countingWriter{w: f}
	gzw := gzip.NewWriter(cw)
	enc := gob.NewEncoder(gzw)

	hdr := Header{
		Format:  FormatVersion,
		RunID:   w.cfg.RunID,
		Seq:     w.fileSeq,
		Started: now,
		Venues:  w.cfg.Venues,
		Symbols: w.cfg.Symbols,
		Depth:   w.cfg.Depth,
	}
	if err := enc.Encode(hdr); err != nil {
		// Cleanup on an already-failing path: the header error is the one
		// worth reporting, so these are deliberately discarded.
		_ = gzw.Close()
		_ = f.Close()
		return fmt.Errorf("capture: encode header: %w", err)
	}

	w.mu.Lock()
	w.enc, w.gzw, w.cw, w.file = enc, gzw, cw, f
	w.fileHour = now.Truncate(time.Hour)
	w.curName = fname
	w.mu.Unlock()
	return nil
}

// rotate finishes the current file and opens the next one. It must be called
// WITHOUT mu held: it performs I/O, and openFile takes mu itself. The previous
// implementation called it from inside the locked region, which deadlocked on
// the very first rotation because sync.Mutex is not reentrant.
func (w *Writer) rotate() error {
	w.closeCurrent()
	w.fileSeq++ // without this the next file overwrites the last
	return w.openFile()
}

// closeCurrent finishes the gzip stream and closes the file. Closing the gzip
// writer is what makes the file independently decodable, so its error matters
// more than the file close.
func (w *Writer) closeCurrent() {
	w.mu.Lock()
	gzw, f := w.gzw, w.file
	w.enc, w.gzw, w.cw, w.file = nil, nil, nil, nil
	w.mu.Unlock()

	if gzw != nil {
		if err := gzw.Close(); err != nil {
			w.setErr(err)
		}
	}
	if f != nil {
		if err := f.Close(); err != nil {
			w.setErr(err)
		}
	}
}

// setErr records the first write error seen. Later records keep being drained
// and discarded so Offer never wedges on a full channel.
func (w *Writer) setErr(err error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
}

func (w *Writer) drain() {
	defer w.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ev, ok := <-w.drainCh:
			if !ok {
				// Channel closed; write final gap and close.
				w.writeFinalGap()
				w.closeCurrent()
				return
			}
			w.writeEvent(ev)
		case <-ticker.C:
			w.flush()
		}
	}
}

func (w *Writer) writeEvent(ev model.Event) {
	// Atomically swap pending drop counter to zero and record gap if any.
	dropped := w.dropCnt.Swap(0)
	if dropped > 0 {
		if err := w.encodeRecord(Record{Wall: w.cfg.Now(), Gap: dropped}); err != nil {
			return
		}
	}
	if err := w.encodeRecord(Record{Wall: w.cfg.Now(), Event: ev}); err != nil {
		return
	}
}

func (w *Writer) writeFinalGap() {
	dropped := w.dropCnt.Swap(0)
	if dropped > 0 {
		// Losing the final gap marker means the tape under-reports its own
		// holes, which is the one failure this package exists to prevent —
		// surface it through Close rather than swallowing it.
		if err := w.encodeRecord(Record{Wall: w.cfg.Now(), Gap: dropped}); err != nil {
			w.setErr(err)
		}
	}
}

// encodeRecord writes one record, rotating first if the hour turned over or
// the file grew past MaxBytes. Runs only on the drain goroutine.
func (w *Writer) encodeRecord(r Record) error {
	now := w.cfg.Now()

	w.mu.Lock()
	needRotate := w.enc != nil &&
		(now.Truncate(time.Hour) != w.fileHour || w.cw.n >= w.cfg.MaxBytes)
	err := w.err
	w.mu.Unlock()

	if err != nil {
		return err
	}
	if needRotate {
		if err := w.rotate(); err != nil {
			w.setErr(err)
			return err
		}
	}

	w.mu.Lock()
	enc := w.enc
	w.mu.Unlock()
	if enc == nil {
		return errors.New("capture: writer is closed")
	}
	if err := enc.Encode(r); err != nil {
		w.setErr(err)
		return err
	}

	w.mu.Lock()
	w.records++
	w.mu.Unlock()
	return nil
}

// flush pushes buffered gzip output to disk so a crash loses at most one
// flush interval, and so a live capture stays readable while it is running.
func (w *Writer) flush() {
	w.mu.Lock()
	gzw := w.gzw
	w.mu.Unlock()
	if gzw == nil {
		return
	}
	// A failing flush means the tape is silently stopping; record it so Close
	// reports it instead of returning success over a truncated file.
	if err := gzw.Flush(); err != nil {
		w.setErr(err)
	}
}

// RunInfo contains metadata for a recorded run.
type RunInfo struct {
	RunID   string
	Files   int
	Started time.Time
	Header  Header
}

// Runs returns information about all runs found in dir, sorted by start time.
// parseCaptureName splits "cap-<runID>-<seq>.gob.gz" into its parts.
//
// It splits on the LAST hyphen, not the first: a RunID is itself
// hyphen-bearing (date-time-random), so any scan that stops at the first
// hyphen reads the date as the whole RunID and then fails to find the
// sequence number.
func parseCaptureName(base string) (runID string, seq int, err error) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(base, "cap-"), ".gob.gz")
	if trimmed == base {
		return "", 0, fmt.Errorf("capture: %q is not a capture file name", base)
	}
	i := strings.LastIndex(trimmed, "-")
	if i < 0 {
		return "", 0, fmt.Errorf("capture: %q has no sequence suffix", base)
	}
	seq, err = strconv.Atoi(trimmed[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("capture: parse seq from %q: %w", base, err)
	}
	return trimmed[:i], seq, nil
}

func Runs(dir string) ([]RunInfo, error) {
	// Match on the prefix/suffix only and let parseCaptureName validate:
	// a glob that hardcodes a hyphen count silently misses runs whose RunID
	// has a different shape.
	pattern := filepath.Join(dir, "cap-*.gob.gz")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("capture: glob: %w", err)
	}
	if len(matches) == 0 {
		return nil, nil
	}

	type fileInfo struct {
		path string
		seq  int
	}
	byRun := make(map[string][]fileInfo)
	for _, p := range matches {
		runID, seq, err := parseCaptureName(filepath.Base(p))
		if err != nil {
			// A file matching the capture glob that does not parse is a
			// problem to surface, not to skip: silently ignoring it would
			// hide a whole run and report a short tape as a complete one.
			return nil, err
		}
		byRun[runID] = append(byRun[runID], fileInfo{p, seq})
	}

	var runs []RunInfo
	for runID, files := range byRun {
		// Sort by seq to verify contiguity.
		sort.Slice(files, func(i, j int) bool { return files[i].seq < files[j].seq })
		for i, f := range files {
			if f.seq != i {
				return nil, fmt.Errorf("capture: run %s missing sequence %d", runID, i)
			}
		}
		// Decode header from first file.
		hdr, err := decodeHeader(files[0].path)
		if err != nil {
			return nil, fmt.Errorf("capture: decode header for run %s: %w", runID, err)
		}
		runs = append(runs, RunInfo{
			RunID:   runID,
			Files:   len(files),
			Started: hdr.Started,
			Header:  hdr,
		})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Started.Before(runs[j].Started) })
	return runs, nil
}

func decodeHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	defer func() { _ = f.Close() }() // read path: close error is not actionable
	gzr, err := gzip.NewReader(f)
	if err != nil {
		return Header{}, err
	}
	defer func() { _ = gzr.Close() }()
	dec := gob.NewDecoder(gzr)
	var hdr Header
	if err := dec.Decode(&hdr); err != nil {
		return Header{}, err
	}
	return hdr, nil
}

// Replay contains statistics from a replay session.
type Replay struct {
	Records   uint64
	Gaps      uint64
	GapEvents uint64
	Truncated bool
}

// ReplayRun replays all records for runID in dir, calling fn for each.
// It returns an error if the sequence is non-contiguous or a non-final file
// is corrupted.
func ReplayRun(dir, runID string, fn func(Record) error) (Replay, error) {
	pattern := filepath.Join(dir, fmt.Sprintf("cap-%s-*.gob.gz", runID))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return Replay{}, fmt.Errorf("capture: glob: %w", err)
	}
	if len(matches) == 0 {
		return Replay{}, fmt.Errorf("capture: no files for run %s", runID)
	}

	type fileEntry struct {
		path string
		seq  int
	}
	// Parse the sequence by stripping the KNOWN prefix rather than scanning
	// for the first hyphen: RunIDs contain hyphens themselves, so a
	// "cap-%*[^-]-%d" scan would stop inside the RunID and misread the seq.
	prefix := fmt.Sprintf("cap-%s-", runID)
	var files []fileEntry
	for _, p := range matches {
		base := filepath.Base(p)
		digits := strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".gob.gz")
		s, err := strconv.Atoi(digits)
		if err != nil {
			return Replay{}, fmt.Errorf("capture: parse seq from %s: %w", p, err)
		}
		files = append(files, fileEntry{p, s})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].seq < files[j].seq })
	for i, f := range files {
		if f.seq != i {
			return Replay{}, fmt.Errorf("capture: missing sequence %d in run %s", i, runID)
		}
	}

	var replay Replay
	for i, f := range files {
		isLast := i == len(files)-1
		truncated, err := replayFile(f.path, isLast, &replay, fn)
		if err != nil {
			return replay, err
		}
		if truncated {
			replay.Truncated = true
			break
		}
	}
	return replay, nil
}

func replayFile(path string, isLast bool, replay *Replay, fn func(Record) error) (truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }() // read path: close error is not actionable
	gzr, err := gzip.NewReader(f)
	if err != nil {
		if isLast && errors.Is(err, gzip.ErrHeader) {
			return true, nil
		}
		return false, err
	}
	defer func() { _ = gzr.Close() }()
	dec := gob.NewDecoder(gzr)

	// Decode and validate header.
	var hdr Header
	if err := dec.Decode(&hdr); err != nil {
		if isLast && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, gzip.ErrChecksum)) {
			return true, nil
		}
		return false, err
	}
	if hdr.Format != FormatVersion {
		return false, fmt.Errorf("capture: unsupported format version %d", hdr.Format)
	}
	if hdr.RunID == "" {
		return false, errors.New("capture: empty RunID in header")
	}

	for {
		var r Record
		if err := dec.Decode(&r); err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			if isLast && (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, gzip.ErrChecksum)) {
				return true, nil
			}
			return false, err
		}
		replay.Records++
		if r.Gap > 0 {
			replay.Gaps++
			replay.GapEvents += r.Gap
		}
		if err := fn(r); err != nil {
			return false, err
		}
	}
}
