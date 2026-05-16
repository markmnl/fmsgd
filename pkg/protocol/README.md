# pkg/protocol

This package implements the fmsg wire protocol encoding and hashing as defined in [SPEC.md](../../SPEC.md).

## Import

```go
import "github.com/markmnl/fmsgd/pkg/protocol"
```

## What it provides

- **Types**: `FMsgHeader`, `FMsgAddress`, `FMsgAttachmentHeader`
- **Flag constants**: `FlagHasPid`, `FlagHasAddTo`, `FlagCommonType`, `FlagImportant`, `FlagNoReply`, `FlagDeflate`
- **Wire encoding**: `FMsgHeader.Encode()` — serialises a header to the exact byte sequence defined in SPEC.md
- **Hashing**: `FMsgHeader.GetHeaderHash()` — SHA-256 of the encoded header; `FMsgHeader.GetMessageHash()` — SHA-256 of header + decompressed body + decompressed attachments
- **Common media type lookup**: `GetCommonMediaType(id)`, `GetCommonMediaTypeID(mimeType)`

## Example: compute a message hash

```go
h := &protocol.FMsgHeader{
    Version:   1,
    Flags:     0,
    From:      protocol.FMsgAddress{User: "alice", Domain: "example.com"},
    To:        []protocol.FMsgAddress{{User: "bob", Domain: "other.com"}},
    Timestamp: 1700000000.0,
    Topic:     "hello",
    Type:      "text/plain",
    Size:      5,
    Filepath:  "/path/to/body/file",
}
hash, err := h.GetMessageHash()
```

## Notes

- `Encode()` produces fields 1–12 of the fmsg wire format (header through attachment headers). Message data and attachment data follow separately on the wire.
- `GetMessageHash()` hashes over **decompressed** data even when the stored file is zlib-compressed (`FlagDeflate` set). Set `ExpandedSize` accordingly.
- `Filepath` and `FMsgAttachmentHeader.Filepath` must point to readable files on disk for `GetMessageHash()` to succeed.
