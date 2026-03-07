# fmsg Protocol -- Compact SPEC

## Protocol Identity

-   Protocol: fmsg (f-message)
-   Current Version: 1
-   All messages are binary encoded.

------------------------------------------------------------------------

## Wire Format (Field Order -- MUST NOT CHANGE)

1.  version (uint8)
2.  flags (uint8)
3.  pid (parent message SHA256 hash) -- present only if flag bit 0 set
4.  from (length‑prefixed UTF‑8 string)
5.  to_count (uint8)
6.  to addresses (distinct, case‑insensitive)
7.  time (float64 POSIX timestamp)
8.  topic (length‑prefixed UTF‑8 string)
9.  type (indexed common MIME if flag bit 1 set, otherwise UTF‑8 MIME
    string)
10. size (uint32)
11. attachment_count (uint8)
12. attachment headers
13. data (≥ 1 byte)
14. attachment bodies

All multi-byte integers are little-endian.
All strings are prefixed with uint8 length.

------------------------------------------------------------------------

## Invariants

-   Topic is immutable after the first message in a thread and will be 0 size in replies.
-   Root messages MUST NOT include pid.
-   Reply messages MUST include pid.
-   Recipient list MUST contain no duplicates (case-insensitive).
-   Data size MUST match the declared size field.
-   Attachment count MUST fit within uint8 range.

------------------------------------------------------------------------

## Flags (uint8 bitmask)

Bit 0: pid present\
Bit 1: Common MIME index used\
Bit 2: Important indicator\
Bit 3: No reply allowed\
Bit 4: Skip mandatory challenge\
Bit 5: Deflate compression applied to data\
Bit 6: Keep-alive requested\
Bit 7: Under duress indicator

------------------------------------------------------------------------

## Domain Resolution (Authoritative Host Discovery)

1.  Resolve `_fmsg.<domain>`.
2.  Only A and AAAA records are authoritative.
3.  Follow CNAME chains until A/AAAA records obtained.
4.  DNSSEC validation SHOULD be performed.
5.  If DNSSEC validation fails → connection MUST terminate.

### Sender IP Verification

Before issuing a CHALLENGE:

-   Independently resolve `_fmsg.<sender-domain>`.
-   Verify incoming IP ∈ authorised IP set.
-   If not authorised → terminate without challenge.

Infrastructure MUST preserve the true originating IP address.

------------------------------------------------------------------------

## Challenge / Response

-   CHALLENGE uses version 255.
-   Receiver issues challenge unless skip flag set.
-   Sender responds with cryptographic hash over canonical message.
-   Incorrect or missing response → reject.
-   Accept/Reject codes 100 and above are returned per recipient; otherwise one Reject code for all recipients on receiver's  domain.

------------------------------------------------------------------------

## Rejection Conditions (MUST Reject)

-   Malformed structure
-   Invalid field ordering
-   Size mismatch
-   Duplicate recipients
-   Unauthorised sender IP
-   DNSSEC validation failure
-   Invalid pid reference

------------------------------------------------------------------------

## Agent Enforcement Rules

When generating or modifying code:

-   Always serialize and parse fields exactly in defined order.
-   Never use TXT, MX, or SRV for host discovery.
-   Always resolve `_fmsg.<domain>` using A/AAAA (with CNAME support).
-   Enforce recipient uniqueness.
-   Validate sender IP before issuing CHALLENGE.
-   Terminate immediately on DNSSEC failure.
-   Respect all flag semantics strictly.


## Reject or Accept Response
A code less than 100 indicates rejection for all recipients and will be the only value. Other codes are per recipient in the same order as the as in the to field of the message excluding recipients for other domains.

name	type	comment
codes	byte array	a single or sequence of unit8 codes
code	name	description
1	invalid	the message is malformed, i.e. not in spec, and cannot be decoded
2	unsupported version	the version is not supported by the receiving host
3	undisclosed	no reason is given
4	too big	total size exceeds host's maximum permitted size of messages
5	insufficent resources	such as disk space to store the message
6	parent not found	parent referenced by pid not found
7	past time	timestamp is too far in the past for this host to accept
8	future time	timestamp is too far in the future for this host to accept
9	time travel	timestamp is before parent timestamp
10	duplicate	message has already been received
11	must challenge	no challenge was requested but is required
12	cannot challenge	challenge was requested by sender but receiver is configured not to
100	user unknown	the recipient message is addressed to is unknown by this host
101	user full	insufficent resources for specific recipient
200	accept	message received, no more data
