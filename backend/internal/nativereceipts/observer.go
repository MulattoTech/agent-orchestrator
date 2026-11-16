package nativereceipts

import (
	"bufio"
	"context"
	"errors"
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
}
type Observer struct {
	mu      sync.Mutex
	roots   []string
	sink    Sink
	cursors map[string]*cursor
	next    int
	status  SourceStatus
}

func NewObserver(roots []string, sink Sink) *Observer {
	return &Observer{roots: append([]string(nil), roots...), sink: sink, cursors: map[string]*cursor{}}
}
func (o *Observer) Snapshot() SourceStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.status
	s.Errors = append([]string(nil), s.Errors...)
	return s
}

// Sweep reads at most 32MiB, 64 files and 20,000 directory entries. Cursor
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
	type found struct {
		path    string
		binding Binding
	}
	var files []found
	entries := 0
	for _, root := range o.roots {
		if len(owned) == 0 {
			break
		}
		root, err := filepath.EvalSymlinks(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			s.Errors = append(s.Errors, "native_root_unavailable")
			continue
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			entries++
			if entries > 20000 {
				s.PendingBytes = true
				return fs.SkipAll
			}
			if walkErr != nil {
				return walkErr
			}
			// Never follow interior links into an unrelated transcript tree.
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			name := d.Name()
			if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") || len(name) < 43 {
				return nil
			}
			id := strings.TrimSuffix(name, ".jsonl")
			id = id[len(id)-36:]
			if b, ok := owned[id]; ok {
				files = append(files, found{path, b})
			}
			return nil
		})
		if err != nil {
			s.Errors = append(s.Errors, "native_discovery_incomplete")
		}
		if entries > 20000 {
			break
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	s.MatchedFiles = len(files)
	budget := int64(32 << 20)
	processed := 0
	for i := 0; i < len(files) && processed < 64 && budget > 0; i++ {
		index := (o.next + i) % len(files)
		f := files[index]
		n, pending, err := o.read(ctx, f.path, f.binding, min(budget, 8<<20), &s)
		budget -= n
		s.BytesRead += n
		s.PendingBytes = s.PendingBytes || pending
		processed++
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
		o.next = (o.next + processed) % len(files)
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
func (o *Observer) read(ctx context.Context, path string, b Binding, budget int64, s *SourceStatus) (int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, false, errors.New("native_file_not_regular")
	}
	c := o.cursors[path]
	if c == nil || !os.SameFile(c.info, info) || info.Size() < c.offset || c.binding != b {
		c = &cursor{binding: b, info: info, parser: NewParser(b)}
		o.cursors[path] = c
	}
	accepted, retryErr := c.parser.RetryPending(ctx, o.sink)
	s.ObservedReceipts += int64(accepted)
	if retryErr != nil {
		return 0, true, retryErr
	}
	if _, err = f.Seek(c.offset, io.SeekStart); err != nil {
		return 0, false, err
	}
	r := bufio.NewReaderSize(io.LimitReader(f, budget), 64<<10)
	var read int64
	for read < budget {
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
