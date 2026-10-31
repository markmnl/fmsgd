package emoji

import "testing"

func TestIsRGI(t *testing.T) {
	for _, s := range []string{
		"👍",
		"❤️",
		"👍🏽",
		"🇳🇿",
		"👨‍👩‍👧",
		"#️⃣",
		"🏴󠁧󠁢󠁳󠁣󠁴󠁿",
	} {
		if !IsRGI(s) {
			t.Errorf("IsRGI(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"",
		"a",
		"👍👍",
		"👍 ",
		"❤", // unqualified: U+2764 without VS16
		"1",
	} {
		if IsRGI(s) {
			t.Errorf("IsRGI(%q) = true, want false", s)
		}
	}
}

func TestIsPossible(t *testing.T) {
	for _, s := range []string{
		"👍",
		"❤️",
		"👍🏽",
		"🇳🇿",
		"👨‍👩‍👧",
		"#️⃣",
		"🏴󠁧󠁢󠁳󠁣󠁴󠁿",
		"🧑‍🦯", // any well-formed ZWJ sequence
	} {
		if !IsPossible(s) {
			t.Errorf("IsPossible(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"",
		"a",
		"1",           // text-default without VS16
		"❤",           // text-default without VS16
		"👍👍",          // two emoji
		"👍 ",          // trailing space
		"👍‍",          // dangling ZWJ
		"‍👍",          // leading ZWJ
		"🇳",           // lone regional indicator
		"🇳🇿🇳",         // three regional indicators
		"a🏽",          // modifier on a non-emoji
		"👍🏽🏽",         // double modifier
		"🏳\U000E007F", // tag on a non-black-flag base
		"\xff",        // invalid UTF-8
	} {
		if IsPossible(s) {
			t.Errorf("IsPossible(%q) = true, want false", s)
		}
	}
}

func TestIsSingleRejectsOversize(t *testing.T) {
	s := ""
	for len(s) <= maxReaction {
		s += "‍👍"
	}
	if IsSingle(s) {
		t.Errorf("IsSingle accepted %d-byte sequence", len(s))
	}
}
