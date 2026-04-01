# fmsgd Project Instructions

## Specification

The fmsg protocol is defined in [SPEC.md](SPEC.md). You MUST read SPEC.md before making any changes to code that handles message parsing, serialisation, flags, protocol steps, challenge/response, domain resolution, or reject/accept response codes.

All code MUST conform to the specification. When in doubt, re-read SPEC.md and follow it exactly.

## Key Rules

- Serialize and parse wire fields in the exact order defined in SPEC.md.
- Use the flag bit assignments from SPEC.md (bit 0 = has pid, bit 1 = has add to, bit 2 = common type, etc.).
- Enforce recipient uniqueness across both to and add to (case-insensitive).
- Reject/accept response codes must match SPEC.md — do not invent new codes.
- Resolve `_fmsg.<domain>` using A/AAAA records only (never TXT, MX, or SRV).
- Validate sender IP before issuing CHALLENGE.
- Connection 2 for challenge MUST target the same IP as Connection 1.
- Topic field is only present on the wire when pid is absent.

## Build & Test

- Language: Go
- Module path: `src/`
- Build: `cd src && go build ./...`
- Test: `cd src && go test ./...`
