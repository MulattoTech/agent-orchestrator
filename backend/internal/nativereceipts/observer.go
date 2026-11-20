package nativereceipts

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard"
	"github.com/google/uuid"
)

type SourceStatus struct {
	At                   time.Time `json:"at"`
	OwnedThreads         int       `json:"ownedThreads"`
	MatchedFiles         int       `json:"matchedFiles"`
	DirectoryEntries     int       `json:"directoryEntries"`
	DiscoveryPending     bool      `json:"discoveryPending"`
	ObservedReceipts     int64     `json:"observedReceipts"`
	UnattributedReceipts int64     `json:"unattributedReceipts"`
	BytesRead            int64     `json:"bytesRead"`
	PendingBytes         bool      `json:"pendingBytes"`
	Errors               []string  `json:"errors"`
	Confidence           string    `json:"confidence"`
}
type cursor struct {
	binding Binding
	info    fs.FileInfo
	offset  int64
	parser  *Parser
	// Bind all complete lines offered to the parser, including a failed line
	// whose replay position has not advanced. Retry must not hash it twice.
	prefixEnd    int64
	prefixHash   hash.Hash
	verifyOffset int64
	verifyHash   hash.Hash
}
type Observer struct {
	mu      sync.Mutex
	roots   []string
	sink    Sink
	cursors map[string]*cursor
	next    int
	status  SourceStatus
	files   map[string]Binding
	walk    *discoveryWalk
	closed  bool
}

func NewObserver(roots []string, sink Sink) *Observer {
	return &Observer{roots: append([]string(nil), roots...), sink: sink, cursors: map[string]*cursor{}, files: map[string]Binding{}}
}
func (o *Observer) Snapshot() SourceStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.status
	s.Errors = append([]string(nil), s.Errors...)
	return s
}

// Sweep reads at most 32MiB, 64 files and 20,000 directory entries. Directory
// discovery resumes across sweeps, including roots after a large history. Cursor
// checkpoints are in memory; restart replay is safe because receipts are
// immutable and keyed by native response ID in the shared durable authority.
func (o *Observer) Sweep(ctx context.Context, bindings []Binding) SourceStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := SourceStatus{At: time.Now().UTC(), Confidence: "partial; only verified AO-bound response receipts; subscription allowance and cost UNKNOWN",
		ObservedReceipts: o.status.ObservedReceipts, UnattributedReceipts: o.status.UnattributedReceipts}
	owned := map[string]Binding{}
	conflicted := map[string]bool{}
	for _, b := range bindings {
		if conflicted[b.Thread] {
			continue
		}
		id, e := uuid.Parse(b.Thread)
		if e != nil || id.String() != b.Thread || b.Session == "" {
			continue
		}
		if old, ok := owned[b.Thread]; ok && old.Session != b.Session {
			s.Errors = append(s.Errors, "conflicting_thread_ownership")
			delete(owned, b.Thread)
			conflicted[b.Thread] = true
			continue
		}
		owned[b.Thread] = b
	}
	s.OwnedThreads = len(owned)
	if o.closed {
		s.Errors = append(s.Errors, "native_observer_closed")
		o.status = s
		return s
	}
	// Drop revoked bindings immediately; ownership may change between sweeps.
	for path, b := range o.files {
		if current, ok := owned[b.Thread]; ok {
			o.files[path] = current
		} else {
			delete(o.files, path)
			delete(o.cursors, path)
		}
	}
	if len(owned) == 0 {
		o.closeWalk()
	} else {
		o.discover(ctx, owned, &s)
	}
	type found struct {
		path    string
		binding Binding
	}
	files := make([]found, 0, len(o.files))
	for path, b := range o.files {
		files = append(files, found{path, b})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	s.MatchedFiles = len(files)
	budget := int64(32 << 20)
	processed := 0
	nextFile := o.next
	for i := 0; i < len(files) && processed < 64 && budget > 0; i++ {
		index := (o.next + i) % len(files)
		f := files[index]
		n, pending, err := o.read(ctx, f.path, f.binding, min(budget, 8<<20), &s)
		budget -= n
		s.BytesRead += n
		s.PendingBytes = s.PendingBytes || pending
		processed++
		nextFile = (index + 1) % len(files)
		if budget == 0 && pending {
			// A partial final record may need a full per-file budget next sweep.
			// Retrying from the first file can starve it behind unfinished files.
			nextFile = index
		}
		if err != nil {
			if errors.Is(err, ErrWrongThread) {
				s.Errors = append(s.Errors, "native_thread_mismatch")
			} else {
				s.Errors = append(s.Errors, "native_receipt_read_incomplete")
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	if len(files) > 0 {
		o.next = nextFile
	}
	if processed < len(files) {
		s.PendingBytes = true
	}
	// Bound diagnostic output; absence of metrics remains visible rather than zero cost.
	if len(s.Errors) > 20 {
		s.Errors = s.Errors[:20]
	}
	o.status = s
	result := s
	result.Errors = append([]string(nil), s.Errors...)
	return result
}

const maxDiscoveryEntries = 20000
const maxDiscoveryDepth = 64

type discoveryDirectory struct {
	path string
	file *os.File
}
type discoveryWalk struct {
	nextRoot int
	stack    []discoveryDirectory
	seen     map[string]bool
}

// Close releases any directory handles retained by a bounded discovery pass.
// It is safe after a canceled sweep and prevents subsequent observation.
func (o *Observer) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closeWalk()
	o.closed = true
}
func (o *Observer) closeWalk() {
	if o.walk != nil {
		for _, d := range o.walk.stack {
			_ = d.file.Close()
		}
		o.walk = nil
	}
}

// discover uses streaming directory enumeration instead of WalkDir, which sorts
// and reads whole directories and restarts at the same prefix on every sweep.
// At most 64 directory handles survive a sweep; no foreign transcript data or
// unbounded directory-entry lists are retained. Known owned files remain readable
// while a long discovery pass advances toward later entries and roots.
func (o *Observer) discover(ctx context.Context, owned map[string]Binding, s *SourceStatus) {
	if o.walk == nil {
		o.walk = &discoveryWalk{seen: map[string]bool{}}
	}
	w := o.walk
	for s.DirectoryEntries < maxDiscoveryEntries {
		if ctx.Err() != nil {
			s.Errors = append(s.Errors, "native_discovery_incomplete")
			break
		}
		if len(w.stack) == 0 {
			if w.nextRoot == len(o.roots) {
				// Only a complete pass retires paths no longer present. A budget
				// boundary cannot discard later files that are already observed.
				for path := range o.files {
					if !w.seen[path] {
						delete(o.files, path)
						delete(o.cursors, path)
					}
				}
				o.walk = nil
				return
			}
			root := o.roots[w.nextRoot]
			w.nextRoot++
			s.DirectoryEntries++
			root, err := filepath.EvalSymlinks(root)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				s.Errors = append(s.Errors, "native_root_unavailable")
				continue
			}
			f, err := os.Open(root)
			if err != nil {
				s.Errors = append(s.Errors, "native_root_unavailable")
				continue
			}
			info, err := f.Stat()
			if err != nil || !info.IsDir() {
				_ = f.Close()
				s.Errors = append(s.Errors, "native_root_unavailable")
				continue
			}
			w.stack = append(w.stack, discoveryDirectory{root, f})
			continue
		}
		dir := w.stack[len(w.stack)-1]
		entries, err := dir.file.ReadDir(1)
		if len(entries) == 0 {
			_ = dir.file.Close()
			w.stack = w.stack[:len(w.stack)-1]
			if err != nil && err != io.EOF {
				s.Errors = append(s.Errors, "native_discovery_incomplete")
			}
			continue
		}
		s.DirectoryEntries++
		d := entries[0]
		// Never follow interior links into an unrelated transcript tree.
		if d.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir.path, d.Name())
		if d.IsDir() {
			if len(w.stack) >= maxDiscoveryDepth {
				s.Errors = append(s.Errors, "native_discovery_depth_limited")
				continue
			}
			before, statErr := d.Info()
			f, openErr := os.Open(path)
			if statErr != nil || openErr != nil {
				if f != nil {
					_ = f.Close()
				}
				s.Errors = append(s.Errors, "native_discovery_incomplete")
				continue
			}
			after, statErr := f.Stat()
			if statErr != nil || !after.IsDir() || before.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) {
				_ = f.Close()
				s.Errors = append(s.Errors, "native_discovery_changed")
				continue
			}
			w.stack = append(w.stack, discoveryDirectory{path, f})
			continue
		}
		if !d.Type().IsRegular() {
			continue
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") || len(name) < 43 {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		id = id[len(id)-36:]
		if b, ok := owned[id]; ok {
			o.files[path] = b
			w.seen[path] = true
		}
	}
	s.DiscoveryPending = o.walk != nil
	s.PendingBytes = s.PendingBytes || s.DiscoveryPending
}

// countedReceiptReader measures physical reads independently from committed JSONL
// offsets. Partial, oversized and malformed records still consume observation IO.
type countedReceiptReader struct {
	reader io.Reader
	bytes  int64
}

func (r *countedReceiptReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

func (o *Observer) read(ctx context.Context, path string, b Binding, budget int64, s *SourceStatus) (bytesRead int64, pending bool, resultErr error) {
	if err := ctx.Err(); err != nil {
		return 0, true, err
	}
	if budget <= 0 {
		return 0, true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, false, errors.New("native_file_not_regular")
	}
	counted := &countedReceiptReader{reader: io.LimitReader(f, budget)}
	defer func() { bytesRead = counted.bytes }()
	c := o.cursors[path]
	reset := func() {
		c = &cursor{binding: b, info: info, parser: NewParser(b), prefixHash: sha256.New()}
		o.cursors[path] = c
	}
	// Keep the metadata reset fast path, including the latest size observation.
	// Content verification below also detects quiescent same-metadata rewrites.
	if c == nil || !os.SameFile(c.info, info) || info.Size() < c.offset || c.binding != b ||
		info.Size() < c.info.Size() || (info.Size() == c.info.Size() && !info.ModTime().Equal(c.info.ModTime())) ||
		info.Size() < c.prefixEnd {
		reset()
	}
	c.info = info
	if c.prefixEnd > 0 {
		if c.verifyHash == nil {
			c.verifyHash = sha256.New()
			c.verifyOffset = 0
		}
		if _, err = f.Seek(c.verifyOffset, io.SeekStart); err != nil {
			return 0, true, err
		}
		// Revalidate the whole parser-state prefix before pending retries or
		// continuation. Appends do not extend this target while proof is pending.
		// Half a full file budget leaves room for a maximum-sized JSONL record;
		// all verification reads share the same physical IO charge as tail reads.
		n, verifyErr := io.CopyN(c.verifyHash, counted, min(c.prefixEnd-c.verifyOffset, max(int64(1), budget/2)))
		c.verifyOffset += n
		if verifyErr != nil {
			c.verifyHash = nil
			c.verifyOffset = 0
			return 0, true, verifyErr
		}
		if c.verifyOffset < c.prefixEnd {
			return 0, true, nil
		}
		matches := bytes.Equal(c.verifyHash.Sum(nil), c.prefixHash.Sum(nil))
		c.verifyHash = nil
		c.verifyOffset = 0
		if !matches {
			reset()
		}
	}
	// A full prefix read adds IO and may span sweeps before cached state can be
	// used. It is not an atomic snapshot against concurrent in-place mutation,
	// including changes to already-verified chunks with preserved metadata.
	accepted, retryErr := c.parser.RetryPending(ctx, o.sink)
	s.ObservedReceipts += int64(accepted)
	if retryErr != nil {
		return 0, true, retryErr
	}
	if _, err = f.Seek(c.offset, io.SeekStart); err != nil {
		return 0, false, err
	}
	tailBudget := budget - counted.bytes
	r := bufio.NewReaderSize(counted, 64<<10)
	var read int64
	for read < tailBudget {
		if err = ctx.Err(); err != nil {
			return read, true, err
		}
		var line []byte
		for {
			part, e := r.ReadSlice('\n')
			if len(line)+len(part) > MaxLineBytes {
				return read, true, errors.New("native_record_too_large")
			}
			line = append(line, part...)
			if e == bufio.ErrBufferFull {
				continue
			}
			if e == io.EOF {
				// A writer may still be appending this record. Do not consume partial JSON.
				return read, len(line) > 0 || c.offset < info.Size(), nil
			}
			if e != nil {
				return read, true, e
			}
			break
		}
		end := c.offset + int64(len(line))
		if end > c.prefixEnd {
			_, _ = c.prefixHash.Write(line[max(int64(0), c.prefixEnd-c.offset):])
			c.prefixEnd = end
		}
		accepted, parseErr := c.parser.Line(ctx, line, o.sink)
		if parseErr != nil {
			if errors.Is(parseErr, inferenceguard.ErrUnboundReceipt) {
				// Account identity for old/foreign turns is unknown. Keep those counters
				// out of this account; historical forensic import is a distinct operation.
				s.UnattributedReceipts++
			} else if errors.Is(parseErr, ErrUnknownContext) {
				s.Errors = append(s.Errors, "native_model_effort_unknown")
			} else {
				// Stop before advancing: malformed or conflicting receipts cannot disappear.
				return read, true, parseErr
			}
		}
		if accepted {
			s.ObservedReceipts++
		}
		n := int64(len(line))
		c.offset += n
		read += n
	}
	return read, c.offset < info.Size(), nil
}
