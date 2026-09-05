package sqlitebranchstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"

	"github.com/mackross/agentloom/threads"
)

// V1 stored positions, not item identities. Assign checkpoint identities in
// list order and rebase the WAL above the old head. Keep the old provenance
// columns: a historical turn index cannot reliably identify a surviving turn
// in a parent that may have been edited, recovered, or deleted since branching.
// In particular, source_turn_seq = 0 means unknown, never index + 1.
// All DDL, checkpoint/WAL changes and the version update share the init tx.
func migrateSQLiteBranchV1(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE thread_branches ADD COLUMN source_turn_seq INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, last_seq FROM thread_branches ORDER BY id`)
	if err != nil {
		return err
	}
	type branchHead struct {
		id  string
		seq uint32
	}
	var heads []branchHead
	for rows.Next() {
		var h branchHead
		if err := rows.Scan(&h.id, &h.seq); err != nil {
			rows.Close()
			return err
		}
		heads = append(heads, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, h := range heads {
		if err := migrateSQLiteBranchV1Data(ctx, tx, h.id, h.seq); err != nil {
			return fmt.Errorf("branch %q: %w", h.id, err)
		}
	}
	return nil
}

func migrateSQLiteBranchV1Data(ctx context.Context, tx *sql.Tx, id string, head uint32) error {
	var cp threads.Checkpoint
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT seq, unsafe, snapshot_json FROM thread_checkpoints WHERE branch_id = ?`, id).Scan(&cp.Seq, &cp.Unsafe, &raw); err != nil {
		return fmt.Errorf("read checkpoint: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &cp.Snapshot); err != nil {
		return fmt.Errorf("decode checkpoint: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, op, event_json, created_at FROM thread_wal_events WHERE branch_id = ? ORDER BY seq`, id)
	if err != nil {
		return err
	}
	var wal []threads.WALEvent
	var times []string
	prev := cp.Seq
	for rows.Next() {
		var seq uint32
		var op, raw, created string
		if err := rows.Scan(&seq, &op, &raw, &created); err != nil {
			rows.Close()
			return err
		}
		var ev threads.WALEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			rows.Close()
			return fmt.Errorf("decode wal %d: %w", seq, err)
		}
		if seq != ev.Seq || op != ev.Op {
			rows.Close()
			return fmt.Errorf("wal %d disagrees with its stored sequence/op", seq)
		}
		// Rows covered by the checkpoint are not part of its replay tail.
		if seq <= cp.Seq {
			continue
		}
		if prev == math.MaxUint32 || seq != prev+1 {
			rows.Close()
			return threads.ErrReplayWALSequence
		}
		prev = seq
		wal = append(wal, ev)
		times = append(times, created)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if prev != head {
		return fmt.Errorf("head %d disagrees with checkpoint/WAL head %d", head, prev)
	}
	cp, err = migrateV1Checkpoint(cp, head)
	if err != nil {
		return err
	}
	probe, err := threads.RestoreCheckpoint(cp, threads.RestoreOptions{AllowUnsafe: true})
	if err != nil {
		return err
	}
	var migrated []threads.WALEvent
	var migratedTimes []string
	for i, ev := range wal {
		if err := ctx.Err(); err != nil {
			return err
		}
		converted, keep, err := migrateV1Event(probe, ev)
		if err != nil {
			return fmt.Errorf("wal %d: %w", ev.Seq, err)
		}
		if !keep {
			continue
		}
		if err := probe.ReplayWAL([]threads.WALEvent{converted}); err != nil {
			return fmt.Errorf("replay wal %d: %w", ev.Seq, err)
		}
		migrated = append(migrated, converted)
		migratedTimes = append(migratedTimes, times[i])
	}
	snapshot, err := json.Marshal(cp.Snapshot)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE thread_checkpoints SET seq = ?, snapshot_json = ? WHERE branch_id = ?`, cp.Seq, string(snapshot), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM thread_wal_events WHERE branch_id = ?`, id); err != nil {
		return err
	}
	for i, ev := range migrated {
		encoded, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO thread_wal_events(branch_id, seq, op, event_json, created_at) VALUES(?, ?, ?, ?, ?)`, id, ev.Seq, ev.Op, string(encoded), migratedTimes[i]); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE thread_branches SET last_seq = ?, head_version = head_version + 1 WHERE id = ?`, probe.Seq(), id)
	return err
}

func migrateV1Checkpoint(cp threads.Checkpoint, head uint32) (threads.Checkpoint, error) {
	s := cp.Snapshot
	if s.Version != 1 {
		return threads.Checkpoint{}, fmt.Errorf("expected v1 snapshot, got version %d", s.Version)
	}
	if uint64(len(s.Items)) > math.MaxUint32 {
		return threads.Checkpoint{}, fmt.Errorf("too many checkpoint items")
	}
	// Reserve all historical mutation sequences, including the old WAL tail,
	// before allocating identities for the translated tail. Old branch points
	// could also contain a synthesized send without advancing their sequence.
	cp.Seq = max(head, uint32(len(s.Items)))
	s.Version, s.HeadSeq = 2, cp.Seq
	var items []threads.SnapshotItem
	before := make([]int, len(s.Items))
	after := make([]int, len(s.Items))
	for i, item := range s.Items {
		after[i] = len(items)
		if item.Type == "item_meta" {
			meta, err := v1Metadata(item.Data)
			if err != nil {
				return threads.Checkpoint{}, err
			}
			if len(items) != 0 {
				last := &items[len(items)-1]
				var prior map[string]any
				if last.Metadata != "" {
					if err := json.Unmarshal([]byte(last.Metadata), &prior); err != nil {
						return threads.Checkpoint{}, err
					}
				}
				if prior == nil {
					prior = map[string]any{}
				}
				for k, v := range meta {
					prior[k] = v
				}
				if len(prior) > 0 {
					encoded, err := json.Marshal(prior)
					if err != nil {
						return threads.Checkpoint{}, err
					}
					last.Metadata = string(encoded)
				}
			}
		} else {
			item.Seq = threads.ItemSeq(i + 1)
			items = append(items, item)
		}
		before[i] = len(items) - 1
	}
	remap := func(index int, queued bool) (int, error) {
		if index == -1 {
			return -1, nil
		}
		if index < 0 || index >= len(before) {
			return 0, fmt.Errorf("invalid v1 control index %d", index)
		}
		if queued {
			if after[index] == len(items) {
				return -1, nil
			}
			return after[index], nil
		}
		return before[index], nil
	}
	var err error
	if s.IPIndex, err = remap(s.IPIndex, false); err != nil {
		return threads.Checkpoint{}, err
	}
	if s.QueueStartIndex, err = remap(s.QueueStartIndex, true); err != nil {
		return threads.Checkpoint{}, err
	}
	if s.StreamInsIndex, err = remap(s.StreamInsIndex, false); err != nil {
		return threads.Checkpoint{}, err
	}
	s.Items = items
	cp.Snapshot = s
	return cp, nil
}

func v1Metadata(raw string) (map[string]any, error) {
	var meta map[string]any
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			return nil, fmt.Errorf("decode v1 item metadata: %w", err)
		}
	}
	return meta, nil
}

func migrateV1Event(t interface {
	Seq() uint32
	Snapshot() (threads.ThreadSnapshot, error)
}, ev threads.WALEvent) (threads.WALEvent, bool, error) {
	switch ev.Op {
	case "queue_item", "queue_item_before_send", "append_stream_item":
		if ev.Item.Type == "item_meta" {
			meta, err := v1Metadata(ev.Item.Data)
			if err != nil {
				return ev, false, err
			}
			s, err := t.Snapshot()
			if err != nil {
				return ev, false, err
			}
			target := len(s.Items) - 1
			if ev.Op == "append_stream_item" {
				target = s.StreamInsIndex
			} else if ev.Op == "queue_item_before_send" {
				for i := s.IPIndex + 1; i < len(s.Items); i++ {
					if s.Items[i].Type == "send" {
						target = i - 1
						break
					}
				}
			}
			// V1 allowed empty/orphan metadata nodes. They have no content or
			// request effect; their old sequence remains reserved in the base.
			if target < 0 || len(meta) == 0 {
				return ev, false, nil
			}
			encoded, err := json.Marshal(meta)
			if err != nil {
				return ev, false, err
			}
			ev = threads.WALEvent{Op: "patch_item_metadata", Target: s.Items[target].Seq, Metadata: string(encoded)}
		}
	case "begin_stream", "end_stream":
	default:
		return ev, false, fmt.Errorf("unsupported v1 wal op %q", ev.Op)
	}
	if t.Seq() == math.MaxUint32 {
		return ev, false, fmt.Errorf("migration exhausts thread sequence")
	}
	ev.Seq = t.Seq() + 1
	if ev.Op == "queue_item" || ev.Op == "queue_item_before_send" || ev.Op == "append_stream_item" {
		ev.Item.Seq = threads.ItemSeq(ev.Seq)
	}
	return ev, true, nil
}
