package message

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/markmnl/fmsgd/pkg/fmsg"
)

type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan dest count %d != val count %d", len(dest), len(r.vals))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case **float64:
			*d = r.vals[i].(*float64)
		case *float64:
			*d = r.vals[i].(float64)
		case *string:
			*d = r.vals[i].(string)
		case *[]byte:
			if r.vals[i] == nil {
				*d = nil
			} else {
				*d = append([]byte(nil), r.vals[i].([]byte)...)
			}
		default:
			return fmt.Errorf("unsupported scan type %T", dest[i])
		}
	}
	return nil
}

type fakeAddrRows struct {
	addrs []string
	idx   int
}

func (r *fakeAddrRows) Next() bool {
	if r.idx >= len(r.addrs) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeAddrRows) Scan(dest ...any) error {
	*(dest[0].(*string)) = r.addrs[r.idx-1]
	return nil
}

func (r *fakeAddrRows) Err() error { return nil }
func (r *fakeAddrRows) Close()     {}

type fakeBatchTx struct {
	sentTime       *float64
	msgHash        []byte
	msgPrepared    []byte
	batchFrom      string
	batchTimestamp float64
	batchExisting  []byte
	prevSnapshot   []byte
	prevBatchHash  []byte
	addToAddrs     []string

	execCalls  int
	sealedHash []byte
	sealedWire []byte
}

func (f *fakeBatchTx) QueryRow(_ context.Context, query string, _ ...any) Row {
	switch {
	case strings.Contains(query, "FROM msg WHERE id=$1"):
		return fakeRow{vals: []any{f.sentTime, f.msgHash, f.msgPrepared}}
	case strings.Contains(query, "SELECT add_to_from,time_added,sha256 FROM msg_add_to_batch"):
		return fakeRow{vals: []any{f.batchFrom, f.batchTimestamp, f.batchExisting}}
	case strings.Contains(query, "SELECT wire_message,sha256 FROM msg_add_to_batch"):
		return fakeRow{vals: []any{f.prevSnapshot, f.prevBatchHash}}
	default:
		return fakeRow{err: fmt.Errorf("unexpected QueryRow: %s", query)}
	}
}

func (f *fakeBatchTx) Query(_ context.Context, query string, _ ...any) (Rows, error) {
	if strings.Contains(query, "SELECT addr FROM msg_add_to") {
		return &fakeAddrRows{addrs: f.addToAddrs}, nil
	}
	return nil, fmt.Errorf("unexpected Query: %s", query)
}

func (f *fakeBatchTx) Exec(_ context.Context, query string, args ...any) error {
	if !strings.Contains(query, "UPDATE msg_add_to_batch SET sha256=$2,wire_message=$3") {
		return fmt.Errorf("unexpected Exec: %s", query)
	}
	f.execCalls++
	f.sealedHash = append([]byte(nil), args[1].([]byte)...)
	f.sealedWire = []byte(args[2].(string))
	return nil
}

func prepareTestMessage(t *testing.T, body string) (*fmsg.Header, []byte, []byte) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(src, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	h := &fmsg.Header{
		Version:   1,
		From:      fmsg.Address{User: "alice", Domain: "example.com"},
		To:        []fmsg.Address{{User: "bob", Domain: "example.com"}},
		Timestamp: 1700000000,
		Type:      "text/plain;charset=UTF-8",
		Size:      uint32(len(body)),
		Filepath:  src,
	}
	prepared, _, err := fmsg.Prepare(h)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	hash, err := prepared.GetMessageHash()
	if err != nil {
		t.Fatalf("GetMessageHash: %v", err)
	}
	snapshot, err := fmsg.MarshalPrepared(prepared)
	if err != nil {
		t.Fatalf("MarshalPrepared: %v", err)
	}
	return prepared, hash, snapshot
}

func TestFinalizeBatchVerifiesOriginalPayloadHash(t *testing.T) {
	sent := 1700000000.0
	prepared, msgHash, snapshot := prepareTestMessage(t, "original message payload")

	// Control: intact payload seals cleanly.
	txOK := &fakeBatchTx{
		sentTime:       &sent,
		msgHash:        msgHash,
		msgPrepared:    snapshot,
		batchFrom:      "@alice@example.com",
		batchTimestamp: 1700000010.0,
		addToAddrs:     []string{"@carol@example.com"},
	}
	gotHash, err := FinalizeBatch(context.Background(), txOK, 1, 2)
	if err != nil {
		t.Fatalf("FinalizeBatch intact payload error: %v", err)
	}
	if len(gotHash) != 32 || txOK.execCalls != 1 || !bytes.Equal(txOK.sealedHash, gotHash) {
		t.Fatalf("FinalizeBatch did not seal valid batch (execCalls=%d, hashLen=%d)", txOK.execCalls, len(gotHash))
	}
	if _, err := fmsg.UnmarshalPrepared(txOK.sealedWire, txOK.sealedHash); err != nil {
		t.Fatalf("sealed batch failed verification: %v", err)
	}

	// Corrupt the stored payload file in place (same length, altered bytes).
	origBytes, err := os.ReadFile(prepared.Filepath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte(nil), origBytes...)
	corrupted[0] ^= 0xff
	if err := os.WriteFile(prepared.Filepath, corrupted, 0640); err != nil {
		t.Fatal(err)
	}

	txCorrupt := &fakeBatchTx{
		sentTime:       &sent,
		msgHash:        msgHash,
		msgPrepared:    snapshot,
		batchFrom:      "@alice@example.com",
		batchTimestamp: 1700000020.0,
		addToAddrs:     []string{"@dave@example.com"},
	}
	if _, err := FinalizeBatch(context.Background(), txCorrupt, 1, 3); err == nil {
		t.Fatal("FinalizeBatch accepted corrupted original payload file")
	} else if !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("FinalizeBatch error = %v, want hash mismatch", err)
	}
	if txCorrupt.execCalls != 0 {
		t.Fatalf("FinalizeBatch sealed batch despite corrupted payload (execCalls=%d)", txCorrupt.execCalls)
	}
}

func TestFinalizeBatchVerifiesReceivedBatchPayloadHash(t *testing.T) {
	sent := 1700000000.0
	prepared, msgHash, _ := prepareTestMessage(t, "received via add-to payload")

	// Build a prior received batch snapshot (the msg row has no wire_message).
	prev := prepared.Clone()
	prev.Flags |= fmsg.FlagHasPid | fmsg.FlagHasAddTo
	prev.Pid = msgHash
	prev.Topic = ""
	prev.Timestamp = 1700000005.0
	prev.AddToFrom = &fmsg.Address{User: "alice", Domain: "example.com"}
	prev.AddTo = []fmsg.Address{{User: "carol", Domain: "example.com"}}
	prevHash, err := prev.GetMessageHash()
	if err != nil {
		t.Fatalf("prev batch GetMessageHash: %v", err)
	}
	prevSnapshot, err := fmsg.MarshalPrepared(prev)
	if err != nil {
		t.Fatalf("prev batch MarshalPrepared: %v", err)
	}

	// Control: intact payload via received-batch snapshot seals cleanly.
	txOK := &fakeBatchTx{
		sentTime:       &sent,
		msgHash:        msgHash,
		msgPrepared:    nil, // original arrived via add-to
		prevSnapshot:   prevSnapshot,
		prevBatchHash:  prevHash,
		batchFrom:      "@bob@example.com",
		batchTimestamp: 1700000010.0,
		addToAddrs:     []string{"@dave@example.com"},
	}
	gotHash, err := FinalizeBatch(context.Background(), txOK, 1, 2)
	if err != nil {
		t.Fatalf("FinalizeBatch intact received-batch payload error: %v", err)
	}
	if len(gotHash) != 32 || txOK.execCalls != 1 {
		t.Fatalf("FinalizeBatch did not seal valid batch (execCalls=%d, hashLen=%d)", txOK.execCalls, len(gotHash))
	}

	// Corrupt the stored payload file in place (same length, altered bytes).
	origBytes, err := os.ReadFile(prepared.Filepath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte(nil), origBytes...)
	corrupted[0] ^= 0xff
	if err := os.WriteFile(prepared.Filepath, corrupted, 0640); err != nil {
		t.Fatal(err)
	}

	txCorrupt := &fakeBatchTx{
		sentTime:       &sent,
		msgHash:        msgHash,
		msgPrepared:    nil,
		prevSnapshot:   prevSnapshot,
		prevBatchHash:  prevHash,
		batchFrom:      "@bob@example.com",
		batchTimestamp: 1700000020.0,
		addToAddrs:     []string{"@eve@example.com"},
	}
	if _, err := FinalizeBatch(context.Background(), txCorrupt, 1, 3); err == nil {
		t.Fatal("FinalizeBatch accepted corrupted received-batch payload file")
	} else if !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("FinalizeBatch error = %v, want hash mismatch", err)
	}
	if txCorrupt.execCalls != 0 {
		t.Fatalf("FinalizeBatch sealed batch despite corrupted payload (execCalls=%d)", txCorrupt.execCalls)
	}
}
