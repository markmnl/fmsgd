// Package emoji recognises single emoji for FMSG-005 reactions and FMSG-006
// indicators.
//
// A reaction's data, and an indicator's non-empty data, must be exactly one
// emoji. [IsRGI] tests membership of the Unicode RGI_Emoji set (UTS #51), the
// set senders are required to use. [IsPossible] accepts the wider UTS #51
// possible_emoji grammar, restricted to sequences that present as emoji, so a
// reaction or indicator using a sequence newer than the compiled tables, or a
// minimally-qualified one, is still recognised (FMSG-006 applies the same
// rules and leniency as FMSG-005).
//
// Run `go run ./internal/emoji/gen` from the repository root to refresh the
// tables from unicode.org.
package emoji

import (
	"unicode"
	"unicode/utf8"
)

const (
	zwj          = 0x200D
	vs16         = 0xFE0F
	keycap       = 0x20E3
	tagBegin     = 0xE0020
	tagEnd       = 0xE007E
	tagTerm      = 0xE007F
	riFirst      = 0x1F1E6
	riLast       = 0x1F1FF
	blackFlag    = 0x1F3F4
	maxReaction  = 64 // bytes; FMSG-005 and FMSG-006 size bound
	maxSequenceR = 24 // runes; generous bound for ZWJ sequences
)

// IsRGI reports whether s is exactly one fully-qualified RGI emoji sequence.
func IsRGI(s string) bool {
	_, ok := rgi[s]
	return ok
}

// IsPossible reports whether s is exactly one emoji under the UTS #51
// possible_emoji grammar, requiring emoji presentation: a text-default
// character such as a digit counts only with U+FE0F, a keycap or a modifier.
//
//	possible_emoji  := flag_sequence | zwj_element (ZWJ zwj_element)*
//	flag_sequence   := RI RI
//	zwj_element     := Emoji emoji_modification?
//	emoji_modification := Emoji_Modifier | FE0F 20E3? | tag_modifier
//	tag_modifier    := [E0020-E007E]+ E007F
func IsPossible(s string) bool {
	if s == "" || len(s) > maxReaction || !utf8.ValidString(s) {
		return false
	}
	runes := []rune(s)
	if len(runes) > maxSequenceR {
		return false
	}
	if len(runes) == 2 && isRI(runes[0]) && isRI(runes[1]) {
		return true
	}
	i := 0
	for {
		n, ok := zwjElement(runes[i:])
		if !ok {
			return false
		}
		i += n
		if i == len(runes) {
			return true
		}
		if runes[i] != zwj {
			return false
		}
		i++
		if i == len(runes) {
			return false
		}
	}
}

// IsSingle reports whether s is one emoji by either test.
func IsSingle(s string) bool {
	return IsRGI(s) || IsPossible(s)
}

func isRI(r rune) bool { return r >= riFirst && r <= riLast }

// zwjElement consumes one zwj_element from the front of runes, returning its
// length. The element must present as emoji.
func zwjElement(runes []rune) (int, bool) {
	if len(runes) == 0 || !unicode.Is(propEmoji, runes[0]) || isRI(runes[0]) {
		return 0, false
	}
	base := runes[0]
	presents := unicode.Is(propEmojiPresentation, base)
	i := 1
	if i < len(runes) {
		switch r := runes[i]; {
		case unicode.Is(propEmojiModifier, r):
			if !unicode.Is(propEmojiModifierBase, base) {
				return 0, false
			}
			i++
			presents = true
		case r == vs16:
			i++
			presents = true
			if i < len(runes) && runes[i] == keycap {
				i++
			}
		case r >= tagBegin && r <= tagEnd:
			if base != blackFlag {
				return 0, false
			}
			for i < len(runes) && runes[i] >= tagBegin && runes[i] <= tagEnd {
				i++
			}
			if i >= len(runes) || runes[i] != tagTerm {
				return 0, false
			}
			i++
			presents = true
		}
	}
	return i, presents
}
