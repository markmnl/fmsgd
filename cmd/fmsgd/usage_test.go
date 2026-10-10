package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestStoredSize(t *testing.T) {
	tests := []struct {
		name string
		h    FMsgHeader
		want int64
	}{
		{name: "body only", h: FMsgHeader{Size: 100}, want: 100},
		{name: "compressed body counts expanded", h: FMsgHeader{Flags: FlagDeflate, Size: 40, ExpandedSize: 400}, want: 400},
		{
			name: "attachments included",
			h: FMsgHeader{Size: 100, Attachments: []FMsgAttachmentHeader{
				{Size: 1000},
				{Flags: 1 << 1, Size: 50, ExpandedSize: 5000},
			}},
			want: 100 + 1000 + 5000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := storedSize(&tt.h); got != tt.want {
				t.Fatalf("storedSize = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRecvLimitExceeded(t *testing.T) {
	unlimited := AddressDetail{
		LimitRecvSizeTotal: -1, LimitRecvSizePer1d: -1, LimitRecvCountPer1d: -1,
		RecvSizeTotal: 1 << 40, RecvSizePer1d: 1 << 40, RecvCountPer1d: 1 << 40,
	}
	tests := []struct {
		name   string
		detail AddressDetail
		size   int64
		want   string // substring of the reason, "" when within limits
	}{
		{name: "unlimited", detail: unlimited, size: 1 << 30},
		{name: "count fits", detail: AddressDetail{LimitRecvCountPer1d: 10, RecvCountPer1d: 9, LimitRecvSizePer1d: -1, LimitRecvSizeTotal: -1}, size: 1},
		{name: "count full", detail: AddressDetail{LimitRecvCountPer1d: 10, RecvCountPer1d: 10, LimitRecvSizePer1d: -1, LimitRecvSizeTotal: -1}, size: 1, want: "RecvCountPer1d"},
		{name: "day size fits exactly", detail: AddressDetail{LimitRecvCountPer1d: -1, LimitRecvSizePer1d: 100, RecvSizePer1d: 60, LimitRecvSizeTotal: -1}, size: 40},
		{name: "day size over", detail: AddressDetail{LimitRecvCountPer1d: -1, LimitRecvSizePer1d: 100, RecvSizePer1d: 60, LimitRecvSizeTotal: -1}, size: 41, want: "RecvSizePer1d"},
		{name: "total over", detail: AddressDetail{LimitRecvCountPer1d: -1, LimitRecvSizePer1d: -1, LimitRecvSizeTotal: 1000, RecvSizeTotal: 999}, size: 2, want: "RecvSizeTotal"},
		{name: "zero limit refuses", detail: AddressDetail{LimitRecvCountPer1d: 0, LimitRecvSizePer1d: -1, LimitRecvSizeTotal: -1}, size: 0, want: "RecvCountPer1d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recvLimitExceeded(&tt.detail, tt.size)
			if tt.want == "" && got != "" {
				t.Fatalf("got %q, want within limits", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRecvLimitIncludesAttachments shows a message whose body fits but whose
// attachments take it over the limit is refused: limits are checked against
// the stored size, the same size that is recorded.
func TestRecvLimitIncludesAttachments(t *testing.T) {
	h := FMsgHeader{Size: 10, Attachments: []FMsgAttachmentHeader{{Size: 500}}}
	detail := AddressDetail{LimitRecvCountPer1d: -1, LimitRecvSizePer1d: 100, LimitRecvSizeTotal: -1}
	if recvLimitExceeded(&detail, int64(h.Size)) != "" {
		t.Fatal("body alone should fit")
	}
	if recvLimitExceeded(&detail, storedSize(&h)) == "" {
		t.Fatal("body plus attachments should exceed the daily size limit")
	}
}

func TestRecordRecvUsage(t *testing.T) {
	type tx struct {
		Address string  `json:"address"`
		Ts      float64 `json:"ts"`
		Size    int64   `json:"size"`
	}
	var mu sync.Mutex
	var got []tx
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/fmsgid/recv" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var v tx
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		got = append(got, v)
		mu.Unlock()
	}))
	defer srv.Close()
	origURI := IDURI
	IDURI = srv.URL
	defer func() { IDURI = origURI }()

	// The sender's timestamp is a day old; usage must use the accept time.
	h := FMsgHeader{
		Timestamp:    1_000,
		Flags:        FlagDeflate,
		Size:         20,
		ExpandedSize: 200,
		Attachments:  []FMsgAttachmentHeader{{Size: 30}},
	}
	accepted := []FMsgAddress{{User: "alice", Domain: "example.com"}, {User: "bob", Domain: "example.com"}}
	recordRecvUsage(&h, accepted, 90_000)

	if len(got) != 2 {
		t.Fatalf("posted %d recv stats, want 2", len(got))
	}
	for i, v := range got {
		if v.Address != accepted[i].ToString() || v.Ts != 90_000 || v.Size != 230 {
			t.Fatalf("stat %d = %+v, want %s at 90000 size 230", i, v, accepted[i].ToString())
		}
	}
}
