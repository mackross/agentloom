package durability

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/mackross/agentloom/threads"
)

const (
	fileMagic   = "TDUR"
	fileVersion = 1
	headerSize  = 16
	flagUnsafe  = 1 << 0
)

type FileStore struct {
	path string
	mu   sync.Mutex
}

var _ threads.DurableStore = (*FileStore)(nil)

func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

func (s *FileStore) ReplaceSnapshot(cp threads.Checkpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.replaceSnapshotNoLock(cp); err != nil {
		panic("threads durability replace snapshot failed: " + err.Error())
	}
}

func (s *FileStore) AppendWALDiff(diff []threads.WALEvent) {
	if len(diff) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendWALNoLock(diff); err != nil {
		panic("threads durability append wal diff failed: " + err.Error())
	}
}

func (s *FileStore) Load() (threads.Checkpoint, []threads.WALEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, wal, err := s.loadNoLock()
	if err != nil {
		panic("threads durability load failed: " + err.Error())
	}
	return cp, wal
}

func (s *FileStore) loadNoLock() (threads.Checkpoint, []threads.WALEvent, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return threads.Checkpoint{}, nil, err
	}
	if len(b) < headerSize {
		return threads.Checkpoint{}, nil, fmt.Errorf("invalid durability file: too short")
	}
	if string(b[0:4]) != fileMagic {
		return threads.Checkpoint{}, nil, fmt.Errorf("invalid durability file: bad magic")
	}
	if b[4] != fileVersion {
		return threads.Checkpoint{}, nil, fmt.Errorf("invalid durability file: bad version")
	}

	snapshotLen := int(binary.BigEndian.Uint32(b[8:12]))
	if snapshotLen < 0 || len(b) < headerSize+snapshotLen {
		return threads.Checkpoint{}, nil, fmt.Errorf("invalid durability file: bad snapshot length")
	}

	var snap threads.ThreadSnapshot
	if err := json.Unmarshal(b[headerSize:headerSize+snapshotLen], &snap); err != nil {
		return threads.Checkpoint{}, nil, fmt.Errorf("invalid durability file: bad snapshot json: %w", err)
	}

	cp := threads.Checkpoint{
		Seq:      binary.BigEndian.Uint32(b[12:16]),
		Unsafe:   b[5]&flagUnsafe != 0,
		Snapshot: snap,
	}
	wal, err := decodeWALNoCopy(b[headerSize+snapshotLen:])
	if err != nil {
		return threads.Checkpoint{}, nil, err
	}
	return cp, wal, nil
}

func (s *FileStore) replaceSnapshotNoLock(cp threads.Checkpoint) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}

	snap, err := json.Marshal(cp.Snapshot)
	if err != nil {
		return err
	}

	head := make([]byte, headerSize)
	copy(head[:4], []byte(fileMagic))
	head[4] = fileVersion
	if cp.Unsafe {
		head[5] |= flagUnsafe
	}
	binary.BigEndian.PutUint32(head[8:12], uint32(len(snap)))
	binary.BigEndian.PutUint32(head[12:16], cp.Seq)

	out := make([]byte, 0, headerSize+len(snap))
	out = append(out, head...)
	out = append(out, snap...)

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *FileStore) appendWALNoLock(diff []threads.WALEvent) error {
	encoded, err := encodeWAL(diff)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(encoded)
	return err
}

func encodeWAL(events []threads.WALEvent) ([]byte, error) {
	out := make([]byte, 0, len(events)*64)
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			return nil, err
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out, nil
}

func decodeWALNoCopy(data []byte) ([]threads.WALEvent, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	lines := bytes.Split(trimmed, []byte{'\n'})
	out := make([]threads.WALEvent, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var ev threads.WALEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("invalid durability file: bad wal json: %w", err)
		}
		out = append(out, ev)
	}
	return out, nil
}
