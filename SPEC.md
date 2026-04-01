# fmsg Protocol -- Compact SPEC

## Protocol Identity

-   Protocol: fmsg (fmsg) 
-   Current Version: 1
-   All messages are binary encoded.

------------------------------------------------------------------------

## Wire Format (Field Order -- MUST NOT CHANGE)

1.  version (uint8) -- 1..127 = fmsg version; 128..255 = CHALLENGE (version = 256 - value)
2.  flags (uint8)
3.  [pid] (32 byte SHA-256 hash) -- present only if flag bit 0 set
4.  from (uint8 + UTF-8 address)
5.  to (uint8 count + list of addresses, distinct case-insensitive, at least one)
6.  [add to] (uint8 count + list of addresses) -- present only if flag bit 1 set, distinct case-insensitive, at least one
7.  time (float64 POSIX timestamp)
8.  [topic] (uint8 + UTF-8 string) -- present only when pid is NOT set, may be 0-length
9.  type (uint8 common-type index when flag bit 2 set; otherwise uint8 length + ASCII MIME string per RFC 6838)
10. size (uint32, data size in bytes, 0 or greater)
11. attachment headers (uint8 count + list of attachment headers, count may be 0)
12. data (byte array, length from size field)
13. [attachments data] (sequential byte sequences, boundaries from attachment header sizes)

All multi-byte integers are little-endian.
All strings are prefixed with uint8 length.

------------------------------------------------------------------------

## Invariants

-   Topic is present only on the first message in a thread (when pid absent) and is immutable.
-   When pid exists the entire topic field MUST NOT be included on the wire.
-   Root messages MUST NOT include pid.
-   Reply messages MUST include pid.
-   All recipients (to + add to combined) MUST be distinct (case-insensitive).
-   pid references previous message hash if from was in to of previous message;
    references previous message header hash if from was only in add to.
-   Data size MUST match the declared size field.
-   Attachment count MUST fit within uint8 range.

------------------------------------------------------------------------

## Flags (uint8 bitmask)

Bit 0: has pid -- pid field is present\
Bit 1: has add to -- add to field is included\
Bit 2: common type -- type field is a uint8 index into Common Media Types\
Bit 3: important -- sender indicates message is IMPORTANT\
Bit 4: no reply -- sender indicates any reply will be discarded\
Bit 5: deflate -- message data compressed with zlib/deflate (RFC 1950/1951)\
Bit 6: TBD (reserved)\
Bit 7: TBD (reserved)

------------------------------------------------------------------------

## Address

Format: `@recipient@domain` (leading `@` distinguishes from email).
Encoded as: uint8 size + UTF-8 string.

Recipient part MUST be: UTF-8, Unicode letters/numbers (`\p{L}`, `\p{N}`),
hyphen `-`, underscore `_`, dot `.` non-consecutively and not at start/end,
unique on host (case-insensitive), combined address < 256 bytes.

------------------------------------------------------------------------

## Attachment Header

Each attachment header: flags (uint8), type (uint8 + [ASCII string]), filename (uint8 + UTF-8), size (uint32).

### Attachment Flags

Bit 0: common type -- type is uint8 index into Common Media Types\
Bit 1: deflate -- data compressed with zlib/deflate\
Bits 2-7: TBD (reserved)

### Filename Rules

-   UTF-8, Unicode letters/numbers (`\p{L}`, `\p{N}`)
-   Hyphen `-`, underscore `_`, single space ` `, dot `.` non-consecutively, not at start/end
-   Unique amongst attachments (case-insensitive)
-   Less than 256 bytes

------------------------------------------------------------------------

## Domain Resolution (Authoritative Host Discovery)

1.  Resolve `_fmsg.<domain>` for A and AAAA records (follow CNAME chains).
2.  Only A and AAAA records are authoritative.
3.  DNSSEC validation SHOULD be performed.
4.  If DNSSEC validation fails → connection MUST terminate.

### Sender IP Verification

Receiving Host B MUST resolve `_fmsg.<sender-domain>` and verify the incoming
connection's IP is in the authorised set. If not authorised → TERMINATE
(no reject code, no challenge).

Infrastructure MUST preserve the true originating IP address.

------------------------------------------------------------------------

## Challenge / Response

-   CHALLENGE version byte: 255 = protocol v1, 254 = v2, etc.
-   Challenge consists of: version (uint8) + header hash (32 bytes SHA-256 of message header up to and including attachment headers).
-   Challenge response: msg hash (32 bytes SHA-256 of entire message).
-   Receiver initiates Connection 2 to the **same IP** as Connection 1.
-   Sender verifies header hash matches message being sent; if not → TERMINATE.
-   Response hash MUST be kept and verified against computed hash once message fully downloaded.
-   Whether to challenge is at the discretion of the recipient host (example modes: NEVER, ALWAYS, HAS_NOT_PARTICIPATED, DIFFERENT_DOMAIN).

------------------------------------------------------------------------

## Reject or Accept Response

A code less than 100 indicates rejection or acceptance for all recipients and
will be the only value. Codes ≥ 100 are per recipient in the order they appear
in _to_ then _add to_, excluding recipients for other domains.

| name  | type       | comment                             |
|-------|------------|-------------------------------------|
| codes | byte array | a single or sequence of uint8 codes |

| code | name                   | description                                                        |
|-----:|------------------------|--------------------------------------------------------------------|
| 1    | invalid                | the message header fails verification checks, i.e. not in spec    |
| 2    | unsupported version    | the version is not supported by the receiving host                 |
| 3    | undisclosed            | no reason is given                                                 |
| 4    | too big                | total size exceeds host's maximum permitted size of messages       |
| 5    | insufficient resources | such as disk space to store the message                            |
| 6    | parent not found       | parent referenced by pid SHA-256 not found and is required         |
| 7    | too old                | timestamp is too far in the past for this host to accept           |
| 8    | future time            | timestamp is too far in the future for this host to accept         |
| 9    | time travel            | timestamp is before parent timestamp                               |
| 10   | duplicate              | message has already been received                                  |
| 11   | accept header          | message header received (add-to notification only)                 |
|      |                        |                                                                    |
| 100  | user unknown           | the recipient is unknown by this host                              |
| 101  | user full              | insufficient resources for specific recipient                      |
| 102  | user not accepting     | user is known but not accepting new messages at this time          |
| 103  | user undisclosed       | no reason given (MAY be used instead of 100-102 to avoid disclosure)|
|      |                        |                                                                    |
| 200  | accept                 | message received for recipient                                     |

------------------------------------------------------------------------

## Verifying Message Stored

A **message** is verified stored if: the SHA-256 digest exactly matches a
previously accepted (code 200) message, and that message currently exists and
can be retrieved.

A **message header** is verified stored if: the SHA-256 digest exactly matches
a previously accepted (code 11) message header, and that header currently exists
and can be retrieved.

------------------------------------------------------------------------

## Protocol Steps Configuration

| Variable        | Example Value | Description                                                               |
|-----------------|---------------|---------------------------------------------------------------------------|
| MAX_SIZE        | 1048576       | Maximum allowed total size in bytes (data + all attachment sizes)         |
| MAX_MESSAGE_AGE | 700000        | Maximum age since message time for acceptance (seconds)                   |
| MAX_TIME_SKEW   | 20            | Maximum tolerance for message time ahead of current time (seconds)        |

------------------------------------------------------------------------

## Rejection Conditions (MUST Reject)

-   Cannot decode / malformed structure → TERMINATE
-   Duplicate recipients (to + add to, case-insensitive) → code 1
-   Common type flag set but value has no mapping → code 1
-   add to exists but pid does not → code 1
-   Unauthorised sender IP → TERMINATE
-   DNSSEC validation failure → TERMINATE
-   size + all attachment sizes > MAX_SIZE → code 4
-   Message too old (DELTA > MAX_MESSAGE_AGE) → code 7
-   Message too far in future (|DELTA| > MAX_TIME_SKEW) → code 8
-   pid parent not found (when required per add-to rules) → code 6
-   Parent time ≥ message time → code 9
-   Duplicate message (via challenge hash lookup) → code 10
-   Hash mismatch after full download (challenge was completed) → TERMINATE

------------------------------------------------------------------------

## Agent Enforcement Rules

When generating or modifying code:

-   Always serialize and parse fields exactly in defined order.
-   Never use TXT, MX, or SRV for host discovery.
-   Always resolve `_fmsg.<domain>` using A/AAAA (with CNAME support).
-   Enforce recipient uniqueness across to and add to.
-   Validate sender IP before issuing CHALLENGE.
-   Terminate immediately on DNSSEC failure.
-   Respect all flag semantics strictly (use spec bit assignments).
-   Topic field only present when pid is absent.
-   Common type flag (bit 2) controls type field encoding (single uint8 index vs length-prefixed string).
-   Connection 2 for challenge MUST target the same IP as Connection 1.
