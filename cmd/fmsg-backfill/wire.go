package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/markmnl/fmsgd/pkg/fmsg"
)

// Decode the old wire_header column offline, without network parser state or
// present-day timestamp limits. Round-tripping must reproduce every byte.
func decodeHeader(data []byte) (*fmsg.Header, error) {
	d := headerReader{r: bytes.NewReader(data)}
	h := &fmsg.Header{Version: d.byte(), Flags: d.byte()}
	if h.Flags&fmsg.FlagHasPid != 0 {
		h.Pid = d.take(32)
	}
	h.From = d.address()
	h.To = d.addresses()
	if h.Flags&fmsg.FlagHasAddTo != 0 {
		a := d.address()
		h.AddToFrom = &a
		h.AddTo = d.addresses()
	}
	d.number(&h.Timestamp)
	if h.Flags&fmsg.FlagHasPid == 0 {
		h.Topic = d.text()
	}
	h.Type, h.TypeID = d.mediaType(h.Flags&fmsg.FlagCommonType != 0)
	d.number(&h.Size)
	if h.Flags&fmsg.FlagDeflate != 0 {
		d.number(&h.ExpandedSize)
	}
	count := d.byte()
	for i := 0; i < int(count); i++ {
		a := fmsg.AttachmentHeader{Flags: d.byte()}
		a.Type, a.TypeID = d.mediaType(a.Flags&1 != 0)
		a.Filename = d.text()
		d.number(&a.Size)
		if a.Flags&2 != 0 {
			d.number(&a.ExpandedSize)
		}
		if a.Flags&^uint8(3) != 0 {
			d.err = fmt.Errorf("reserved attachment flags in stored header")
		}
		h.Attachments = append(h.Attachments, a)
	}
	if d.err != nil {
		return nil, fmt.Errorf("invalid stored wire header: %w", d.err)
	}
	if h.Version != 1 || h.Flags&128 != 0 || len(h.To) == 0 ||
		(h.Flags&fmsg.FlagHasAddTo != 0 && (len(h.AddTo) == 0 || len(h.Pid) != 32)) ||
		d.r.Len() != 0 || !bytes.Equal(h.Encode(), data) {
		return nil, fmt.Errorf("stored wire header does not round-trip")
	}
	return h, nil
}

type headerReader struct {
	r   *bytes.Reader
	err error
}

func (d *headerReader) take(n int) []byte {
	b := make([]byte, n)
	if d.err == nil {
		_, d.err = io.ReadFull(d.r, b)
	}
	return b
}
func (d *headerReader) byte() byte   { return d.take(1)[0] }
func (d *headerReader) text() string { return string(d.take(int(d.byte()))) }
func (d *headerReader) number(v any) {
	if d.err == nil {
		d.err = binary.Read(d.r, binary.LittleEndian, v)
	}
}
func (d *headerReader) address() fmsg.Address {
	raw := d.text()
	if d.err != nil {
		return fmsg.Address{}
	}
	a, err := address(raw)
	if err != nil {
		d.err = err
	}
	return a
}
func (d *headerReader) addresses() []fmsg.Address {
	n := int(d.byte())
	list := make([]fmsg.Address, n)
	for i := range list {
		list[i] = d.address()
	}
	return list
}
func (d *headerReader) mediaType(common bool) (string, uint8) {
	if !common {
		return d.text(), 0
	}
	id := d.byte()
	typ, ok := fmsg.GetCommonMediaType(id)
	if !ok {
		d.err = fmt.Errorf("unknown common media type %d", id)
	}
	return typ, id
}

// Previous receivers retained expanded API files and the wire header, but not
// compressed payload files. Recreate a valid stream of the declared size. The
// protocol hashes expanded bytes, so compressed bytes need not be identical;
// the header (including wire size) and expanded bytes must be identical.
func (m *migration) restore(wire, raw *fmsg.Header) (*fmsg.Header, error) {
	h := wire.Clone()
	var temps []string
	defer func() {
		for _, path := range temps {
			_ = os.Remove(path)
		}
	}()
	part := func(path string, rawSize, size, expanded uint32, compressed bool) (string, error) {
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		want := size
		if compressed {
			want = expanded
		}
		if !info.Mode().IsRegular() || info.Size() != int64(want) || rawSize != want {
			return "", fmt.Errorf("stored payload length differs from wire header: %s", path)
		}
		if !compressed {
			return path, nil
		}
		p, err := restoreCompressed(path, size)
		if err == nil {
			temps = append(temps, p)
		}
		return p, err
	}
	var err error
	h.Filepath, err = part(raw.Filepath, raw.Size, h.Size, h.ExpandedSize, h.Flags&fmsg.FlagDeflate != 0)
	if err != nil {
		return nil, err
	}
	if len(h.Attachments) != len(raw.Attachments) {
		return nil, fmt.Errorf("stored attachments differ from wire header")
	}
	for i := range h.Attachments {
		a := &h.Attachments[i]
		found := false
		for _, r := range raw.Attachments {
			if r.Filename == a.Filename {
				a.Filepath, err = part(r.Filepath, r.Size, a.Size, a.ExpandedSize, a.Flags&2 != 0)
				if err != nil {
					return nil, err
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("missing attachment %s", a.Filename)
		}
	}
	h, dir, err := fmsg.Preserve(h, filepath.Dir(raw.Filepath))
	if err == nil {
		m.files = append(m.files, dir)
	}
	return h, err
}

func restoreCompressed(path string, size uint32) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp("", "fmsg-backfill-zlib-*")
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		out.Close()
		if !keep {
			os.Remove(out.Name())
		}
	}()
	for _, level := range []int{zlib.DefaultCompression, 1, 9, 0, zlib.HuffmanOnly, 2, 3, 4, 5, 7, 8} {
		if _, err = in.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		if err = out.Truncate(0); err != nil {
			return "", err
		}
		if _, err = out.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		zw, err := zlib.NewWriterLevel(out, level)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(zw, in)
		err = zw.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if err != nil {
			return "", err
		}
		n, err := out.Seek(0, io.SeekCurrent)
		if err != nil {
			return "", err
		}
		if n == int64(size) {
			keep = true
			return out.Name(), nil
		}
	}
	return "", fmt.Errorf("cannot reconstruct %d-byte compressed representation of %s; restore original wire payload before upgrading", size, path)
}
