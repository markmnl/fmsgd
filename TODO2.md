# TODO2

Gap analysis of the codebase against SPEC.md.

---

## P0 — Wire Format / Encoding (defs.go)

These are foundational: the header hash (used for challenge verification and pid
references) and message hash (used for duplicate detection, challenge response,
and pid) are both wrong until Encode() is complete.

### ~~1. Encode() must include size and attachment headers~~ DONE
**File:** `defs.go` `Encode()`
Spec defines "message header" as all fields through the attachment headers.
Encode() currently stops after the type field — it omits:
  - size (uint32 LE)
  - attachment count (uint8) + attachment headers (flags, type, filename, size)

The header hash (SHA-256 of the encoded header) will be incorrect without these
fields, which breaks challenge verification and pid references.

### 2. Encode() must omit topic when pid is set
**File:** `defs.go` `Encode()`
Spec: "When pid exists the entire topic field MUST NOT be included on the wire."
Encode() always writes the topic. It must only write topic when pid is absent.

### 3. Encode() must support common-type encoding for type field
**File:** `defs.go` `Encode()`
When common-type flag (bit 2) is set, type on the wire is a single uint8 index,
not a length-prefixed string. Encode() always writes length-prefixed.

### ~~4. Add Attachments field to FMsgHeader~~ DONE
**File:** `defs.go` struct
Add `Attachments []FMsgAttachmentHeader` to store parsed attachment headers.
Required before Encode() can include attachment headers, before attachment
parsing/validation/download, and before hash computation is correct.

### ~~5. Complete FMsgAttachmentHeader struct~~ DONE
**File:** `defs.go` struct
Currently only has Filename, Size, Filepath. Missing per-spec fields:
  - Flags (uint8) — including per-attachment common-type (bit 0) and deflate (bit 1)
  - Type (string) — the attachment's media type

### 6. Add ChallengeCompleted flag to FMsgHeader
**File:** `defs.go` struct
Add `ChallengeCompleted bool` to distinguish "challenge was completed and
ChallengeHash is valid" from "challenge was not performed." Without this, the
hash check in downloadMessage erroneously fails when the challenge was skipped.

### ~~7. GetMessageHash() must include attachment data~~ DONE
**File:** `defs.go` `GetMessageHash()`
Spec: message hash is SHA-256 of the entire message — header + data +
attachment data. Currently attachment data (sequential byte sequences following
the message body) is not included.

---

## P1 — Receiving: Header Exchange (host.go readHeader)

### 8. Generalise first-byte version/challenge detection
**File:** `host.go` `readHeader()`
Spec step 1.3: 1..127 = version, 129..255 = CHALLENGE (version = 256 − value),
0 and 128 are undefined. Currently only v==255 is handled as a challenge. Must
handle any value > 128 where (256 − value) is a supported version.

### 9. Send reject code 2 for unsupported version
**File:** `host.go` `readHeader()`
Spec 1.3.iii: Send code 2 on the connection before closing. Currently just
returns an error without sending any code.

### 10. Validate at least one "to" recipient
**File:** `host.go` `readHeader()`
Spec 1.4.i.a: If to count is 0, reject code 1 (invalid). Currently no check.

### 11. Make topic conditional on pid absence
**File:** `host.go` `readHeader()`
Spec: topic field is only present when pid is NOT set. Currently topic is
read unconditionally regardless of pid.

### 12. Handle common-type flag for type field
**File:** `host.go` `readHeader()`
When common-type flag (bit 2) is set, type is a single uint8 index into the
Common Media Types table. If the value has no mapping → reject code 1.
Currently always reads type as a length-prefixed string.

### 13. Parse and validate attachment headers
**File:** `host.go` `readHeader()`
Currently rejects any non-zero attachment count. Must:
  - Parse each header: flags (uint8), type, filename (uint8 + UTF-8), size (uint32).
  - Validate per-attachment common-type flag mapping (reject code 1 if unmapped).
  - Validate filenames per spec (Unicode letters/numbers, limited special chars,
    no consecutive dots, not at start/end, unique case-insensitive, < 256 bytes).
  - Include all attachment sizes in MAX_SIZE check (data size + Σ attachment sizes).
  - Store parsed headers on FMsgHeader.Attachments.

### 14. DNS-verify sender IP during header exchange
**File:** `host.go` `readHeader()`
Spec 1.4.ii / "Sender IP Verification": Host B MUST resolve
`_fmsg.<sender-domain>` and verify the incoming connection's IP is in the
authorised set. If not authorised → TERMINATE (no reject code, no challenge).
Currently this check only happens inside challenge() and is skipped when
challenge is not performed.

### 15. Perform pid verification during header exchange
**File:** `host.go` `readHeader()`
Spec 1.4.v.c: When pid exists and add to does not:
  a. pid must be verified stored per "Verifying Message Stored"; else code 6.
  b. Parent time must be before message time; else code 9 (time travel).
Currently deferred to downloadMessage().

### 16. Distinguish code-200 vs code-11 parents for add-to header-only path
**File:** `host.go` `readHeader()` + `store.go`
Spec 1.4.v.b.a (no add-to recipients for our domain): pid MUST match a message
originally accepted with code 200 (not code 11), preventing add-to chaining.
Currently lookupMsgIdByHash does not distinguish how the message was accepted.

### 17. Verify sender participated in parent message
**File:** `host.go` `readHeader()`
Spec invariant: "A sender (from) MUST have been a participant in the message
referenced by pid." Not currently checked.

---

## P2 — Receiving: Challenge (host.go challenge)

### 18. Connection 2 must target same IP as Connection 1
**File:** `host.go` `challenge()`
Spec 2.1: Dial conn.RemoteAddr() IP, not h.From.Domain. Dialling the domain
may resolve to a different IP.

### 19. Make challenge mode configurable
**File:** `host.go` `challenge()` / `handleConn()`
Spec defines challenge modes (NEVER, ALWAYS, HAS_NOT_PARTICIPATED,
DIFFERENT_DOMAIN) as Host B's implementation choice. Currently always challenges.
At minimum, support skipping the challenge so the ChallengeCompleted guard
(item 6) works correctly.

---

## P3 — Receiving: Download and Response (host.go downloadMessage)

### 20. Pre-download duplicate check via challenge hash
**File:** `host.go` `downloadMessage()`
Spec 3.1: BEFORE downloading data, if challenge was completed, use the message
hash to check for duplicate → code 10. Currently the duplicate check is after
download, wasting bandwidth.

### 21. Guard hash verification behind ChallengeCompleted
**File:** `host.go` `downloadMessage()`
Spec 3.3: Hash verification must only run when challenge was completed. Currently
ChallengeHash is zero-valued when skipped, causing false mismatch on every
non-challenged message.

### 22. Download attachment data
**File:** `host.go` `downloadMessage()`
Spec 3.2: Download sequential attachment byte sequences after message body,
bounded by attachment header sizes. Currently only message body is downloaded.

### 23. Per-recipient user-duplicate check (code 103)
**File:** `host.go` `downloadMessage()` / `validateMsgRecvForAddr()`
Spec 3.4.i ordering: user duplicate (103) → unknown (100) → full (101) →
not accepting (102) → accept (200). Currently there is no check for whether
the message was already received for a specific recipient (code 103).

---

## P4 — Sending (sender.go)

### 24. Write actual attachment headers
**File:** `sender.go` `deliverMessage()`
Attachment count is hardcoded to 0. Must write attachment count + each
attachment header (flags, type, filename, size) from FMsgHeader.Attachments.

### 25. Send attachment data after message body
**File:** `sender.go` `deliverMessage()`
Spec: sequential attachment byte sequences following data, bounded by header
sizes. Currently not sent.

### 26. Implement exponential back-off for retries
**File:** `sender.go` `findPendingTargets()`
Spec says "SHOULD apply a back-off strategy." Currently uses a fixed
RetryInterval.

### 27. Add code 101 (user full) to retryable set
**File:** `sender.go` `findPendingTargets()`
Per-user code 101 is analogous to global code 5 and is likely transient.
Spec says MAY warrant retry.

---

## P5 — Storage (store.go / dd.sql)

### 28. Store and load attachment metadata
**Files:** `store.go` `storeMsgDetail()` / `loadMsg()`, `dd.sql` msg_attachment
  - dd.sql: msg_attachment is missing `flags` (uint8) and `type` (varchar)
    columns needed for wire-format reconstruction and hash computation.
  - storeMsgDetail: insert attachment headers into msg_attachment.
  - loadMsg: load attachment rows into FMsgHeader.Attachments.

### 29. Distinguish acceptance mode in stored messages
**File:** `store.go` / `dd.sql`
For item 16 (preventing add-to chaining), need a way to distinguish messages
accepted via code 200 (full message) from code 11 (header-only add-to
notification). Options: boolean column, separate lookup, or filepath="" check.

---

## P6 — Address Validation (host.go)

### 30. Support Unicode in address recipient part
**File:** `host.go` `isValidUser()`
Spec says recipient may contain Unicode letters/numbers (`\p{L}`, `\p{N}`).
Currently only ASCII a-z, A-Z, 0-9 are accepted. Must use Unicode-aware
checks (e.g. `unicode.IsLetter`, `unicode.IsNumber`).

### 31. Enforce dot placement rules in address recipient part
**File:** `host.go` `isValidUser()`
Spec says dot `.` must not be consecutive and not at start/end. Currently dots
are allowed anywhere with no positional checks.

---

## P7 — DNS / DNSSEC (dns.go)

### 32. Perform DNSSEC validation
**Files:** `dns.go` `lookupAuthorisedIPs()`, `sender.go`
Spec: DNSSEC validation SHOULD be performed. If validation fails → connection
MUST terminate (no retry). Currently not performed or reported.

---

## P8 — Handling a Challenge (host.go)

### 33. Incoming challenge must send reject code 2 for unsupported version
**File:** `host.go` `readHeader()` / `handleChallenge()`
Spec "Handling a Challenge" step 1: if the first byte is not a supported
version and not a valid challenge, send reject code 2 (unsupported version)
and close. Currently handleChallenge is only entered for v==255 and other
unsupported values return an error without sending a code.
