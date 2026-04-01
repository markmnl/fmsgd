# TODO

Items ordered by perceived priority — foundational / blocking items first, then
correctness issues, then enhancements.

---

## P0 — Foundational (blocks most other work)

### 1. Realign flag bit assignments to spec
**File:** `host.go` constants  
Current flags are wrong: `FlagImportant` is on bit 1 (spec: bit 3),
`FlagNoReply` on bit 3 (spec: bit 4), and `FlagSkipChallenge` (bit 4) does not
exist in the spec. Missing: `FlagHasAddTo` (bit 1), `FlagCommonType` (bit 2).
All flag definitions and every usage site must be realigned.

### 2. Fix `Encode()` to produce the full message header per spec
**File:** `defs.go` `Encode()`  
Currently encodes only version through type. Missing: add-to field, size
(uint32), attachment headers (uint8 count + headers). Topic is always encoded
but must be absent when pid is set. Type is always length-prefixed but must be a
single uint8 index when the common-type flag (bit 2) is set. The header hash
(SHA-256 of encoded header) is wrong without these fields, breaking challenge
verification and pid references.

### 3. Add `AddTo` field to `FMsgHeader`
**File:** `defs.go` struct  
Add `AddTo []FMsgAddress` field. Required before any add-to parsing, encoding,
storage, or per-recipient response ordering can work.

### 4. Add `Attachments` field to `FMsgHeader`
**File:** `defs.go` struct  
Add `Attachments []FMsgAttachmentHeader` to store parsed attachment headers
(flags, type, filename, size). Required before attachment parsing, encoding,
size checks, or hash computation can be correct.

### 5. Add `ChallengeCompleted` sentinel to `FMsgHeader`
**File:** `defs.go` struct  
Add a `ChallengeCompleted bool` to distinguish "challenge was completed and
`ChallengeHash` is valid" from "challenge was not performed." Without this the
hash verification check in `downloadMessage` erroneously fails when the
challenge was skipped.

### 6. Fix response code constants
**File:** `host.go` constants  
`RejectCodeMustChallenge` (11) and `RejectCodeCannotChallenge` (12) do not
exist in the spec. Code 11 = "accept header" (add-to notification success).
Add missing per-user codes: 102 (user not accepting), 103 (user undisclosed).

---

## P1 — Receiving path (host.go) correctness

### 7. DNS-verify sender IP during header exchange, not just in challenge
**File:** `host.go` `readHeader()`  
Spec 1.4.ii: Host B MUST resolve `_fmsg.<from-domain>` and verify the incoming
connection's IP is authorised. If not → TERMINATE (no reject code). Currently
this only happens inside `challenge()` and is skipped when skip-challenge is
allowed.

### 8. Parse "add to" field when has-add-to flag is set
**File:** `host.go` `readHeader()`  
Spec 1.4.v.b: Read uint8 count + addresses. Verify distinct from each other and
from "to" (case-insensitive). Implement the pid/add-to decision tree (add-to
requires pid; if no add-to recipients on our domain → accept header code 11).

### 9. Make topic conditional on pid absence
**File:** `host.go` `readHeader()`  
Topic field is only present when pid is NOT set. Currently read unconditionally.

### 10. Handle common-type flag for type field
**File:** `host.go` `readHeader()`  
When common-type flag (bit 2) is set, type is a single uint8 index into the
Common Media Types table. If value has no mapping → reject code 1.

### 11. Parse attachment headers
**File:** `host.go` `readHeader()`  
Parse each attachment header (flags, type, filename, size). Validate filenames,
common-type mappings, uniqueness. Incorporate attachment sizes into MAX_SIZE
check. Currently rejects any non-zero attachment count.

### 12. Move pid verification to header exchange
**File:** `host.go` `readHeader()` / `downloadMessage()`  
Spec 1.4.v.c: When pid exists (with or without add-to), verify stored per
"Verifying Message Stored" (code 200 or 11, data retrievable). Check parent
time < message time (else code 9). Currently deferred to `downloadMessage`.

### 13. Pre-download duplicate check using challenge hash
**File:** `host.go` `downloadMessage()`  
Spec 3.1: Before downloading data, if challenge was completed, use the message
hash to check for duplicates → code 10. Currently duplicate check is after
download, wasting bandwidth.

### 14. Guard hash verification behind ChallengeCompleted
**File:** `host.go` `downloadMessage()`  
Spec 3.3: Hash verification must only run when challenge was completed.
Currently `ChallengeHash` is zero-valued when skipped, causing false mismatch.

### 15. Download attachment data
**File:** `host.go` `downloadMessage()`  
Spec 3.2: Download sequential attachment byte sequences after message body,
bounded by attachment header sizes. Currently only message body is downloaded.

### 16. Include add-to recipients in per-recipient response codes
**File:** `host.go` `downloadMessage()`  
Spec 3.4: Response codes in order of "to" then "add to", excluding other
domains. Currently only "to" is considered.

### 17. Return correct code for "not accepting" users
**File:** `host.go` `validateMsgRecvForAddr()`  
When user is known but not accepting, return code 102 (user not accepting) or
103 (user undisclosed), not 100 (user unknown).

### 18. Validate at least one "to" recipient
**File:** `host.go` `readHeader()`  
Spec 1.4.i.a: If to count is 0, reject code 1 (invalid).

### 19. Send reject code 2 for unsupported version
**File:** `host.go` `readHeader()`  
Spec 1.3.iii: Send code 2 on the connection before closing when version is
unsupported. Currently just returns an error.

### 20. Generalise challenge version byte handling
**File:** `host.go` `readHeader()`  
Any value > 128 where (256 - value) is a supported version is a challenge.
Currently only v==255 is handled.

---

## P2 — Sending path (sender.go) correctness

### 21. Include add-to recipients in domain recipient list
**File:** `sender.go` `deliverMessage()`  
`domainRecips` only iterates `h.To`. Per spec, per-recipient codes arrive in
"to then add to" order. Append add-to recipients on the target domain after to
recipients.

### 22. Write attachment headers and attachment bodies
**File:** `sender.go` `deliverMessage()`  
Attachment count is hardcoded to 0. Write actual attachment headers (flags,
type, filename, size) and send attachment data after message body.

### 23. Handle code 11 (accept header) as success
**File:** `sender.go` `deliverMessage()`  
Code 11 is < 100 but is NOT a rejection — it means "header received" for
add-to-only scenarios. Handle separately from the global rejection branch.
Also treat code 11 as success in per-recipient handling for add-to recipients.

### 24. Handle missing per-user codes 102 and 103
**File:** `sender.go` `deliverMessage()`  
Code 102 (user not accepting) may be transient — consider retry. Code 103 (user
undisclosed) should be handled similarly to code 3.

---

## P3 — Challenge path correctness

### 25. Connection 2 must target same IP as Connection 1
**File:** `host.go` `challenge()`  
Spec 2.1: Dial `conn.RemoteAddr()` IP, not `h.From.Domain`. Dialling the domain
may resolve to a different IP.

### 26. Replace FlagSkipChallenge with implementation-defined challenge mode
**File:** `host.go` `challenge()`  
Spec defines challenge modes (NEVER, ALWAYS, HAS_NOT_PARTICIPATED,
DIFFERENT_DOMAIN) as Host B's choice. `FlagSkipChallenge` is sender-controlled
and not part of the spec.

---

## P4 — Storage layer

### 27. Distinguish to vs add-to in msg_to table
**File:** `store.go` `storeMsgDetail()`  
Need a way (boolean column, separate table, or ordering convention) to tell "to"
from "add to" recipients so `loadMsg` can reconstruct both slices in order.

### 28. Store and load attachment metadata
**File:** `store.go` `storeMsgDetail()` / `loadMsg()`  
Insert attachment headers into `msg_attachment` and load them back into
`FMsgHeader.Attachments` so the sender can write them on the wire and hashing is
correct.

### 29. Load add-to recipients separately in loadMsg
**File:** `store.go` `loadMsg()`  
Currently all recipients go into `FMsgHeader.To`. Populate `.To` and `.AddTo`
separately, preserving wire-format order.

---

## P5 — Hash computation

### 30. Include size + attachment headers in header hash
**File:** `defs.go` `Encode()` / `GetHeaderHash()`  
Once `Encode()` is fixed (item 2), the hash via `GetHeaderHash()` will
automatically be correct.

### 31. Include attachment data in message hash
**File:** `defs.go` `GetMessageHash()`  
Append sequential attachment byte sequences (bounded by header sizes) after the
message body data in the hash computation.

---

## P6 — Retry / delivery robustness

### 32. Implement exponential back-off for retries
**File:** `sender.go` `findPendingTargets()`  
Spec says "SHOULD apply a back-off strategy." Currently uses a fixed
`RetryInterval`.

### 33. Add code 101 (user full) to retryable set
**File:** `sender.go` `findPendingTargets()`  
Per-user code 101 is analogous to global code 5 (insufficient resources) and is
likely transient.

### 34. Clarify behaviour for unlocked recipients in per-recipient responses
**File:** `sender.go` `deliverMessage()`  
When some recipients on a domain are already delivered or locked by another
sender, do we still get per-recipient codes for them? Need to decide whether
to update response codes for non-locked addresses.

---

## P7 — DNS / network

### 35. Perform DNSSEC validation
**File:** `dns.go` / `sender.go`  
Spec: DNSSEC validation SHOULD be performed. If validation fails → TERMINATE
(no retry). `lookupAuthorisedIPs` does not currently perform or report DNSSEC
status.

---

## P8 — Operational

### 36. Ping ID URL on startup
**File:** `host.go` startup  
Verify the FMSG_ID_URL is up and responding in a timely manner before accepting
connections.

---

## Legacy questions (from original TODO.md)

- Should `time_delivered` be on the whole msg since either wholly accepted or
  not?
- What when one recipient accepted and another did not — resend to domain — but
  then will get duplicate?
