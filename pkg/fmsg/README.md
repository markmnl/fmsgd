# fmsg

Go package implementing the [fmsg protocol](../../SPEC.md) message types, wire-format encoding, and SHA-256 hashing.

```
import "github.com/markmnl/fmsgd/pkg/fmsg"
```

## Types

| Type | Description |
|---|---|
| `fmsg.Header` | All fields of an fmsg message header |
| `fmsg.Address` | fmsg address (`@user@domain`) |
| `fmsg.AttachmentHeader` | Wire-level metadata for a single attachment |

## Flag constants

```go
fmsg.FlagHasPid     // bit 0: message is a reply (pid field present)
fmsg.FlagHasAddTo   // bit 1: add-to addresses present
fmsg.FlagCommonType // bit 2: type encoded as common media type ID
fmsg.FlagImportant  // bit 3: sender marks as important
fmsg.FlagNoReply    // bit 4: sender will discard replies
fmsg.FlagDeflate    // bit 5: body is zlib-deflate compressed
```

## Usage

### Encode a header to wire format

```go
h := &fmsg.Header{
    Version:   1,
    From:      fmsg.Address{User: "alice", Domain: "example.com"},
    To:        []fmsg.Address{{User: "bob", Domain: "example.com"}},
    Timestamp: float64(time.Now().UnixMicro()) / 1e6,
    Topic:     "hello",
    Type:      "text/plain",
    Size:      uint32(len(body)),
}
wire := h.Encode()
```

### Hash a message (for verification or storage)

```go
// Set h.Filepath (and each attachment's Filepath) before calling.
hash, err := h.GetMessageHash()
```

### Look up a common media type

```go
mtype, ok := fmsg.GetCommonMediaType(id)   // ID → "text/plain"
id, ok    := fmsg.GetCommonMediaTypeID(mt) // "text/plain" → ID
```
