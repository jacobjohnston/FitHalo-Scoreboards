package password

import "unicode"

// The admin's password limits, counted in characters as a person sees them.
const (
	// MinimumLength is the fewest characters a password may have.
	MinimumLength = 8

	// MaximumLength is the most characters a password may have.
	MaximumLength = 1024
)

// Code points that Length treats specially.
const (
	// zeroWidthJoiner joins the characters either side of it into one.
	zeroWidthJoiner = 0x200D

	// firstRegionalLetter is the first regional indicator; a pair of them spells a flag.
	firstRegionalLetter = 0x1F1E6

	// lastRegionalLetter is the last regional indicator.
	lastRegionalLetter = 0x1F1FF

	// firstSkinTone is the first emoji skin tone modifier.
	firstSkinTone = 0x1F3FB

	// lastSkinTone is the last emoji skin tone modifier.
	lastSkinTone = 0x1F3FF

	// firstTag is the first tag character, used to spell subdivision flags.
	firstTag = 0xE0020

	// lastTag is the last tag character, the cancel tag that ends a tag sequence.
	lastTag = 0xE007F

	// firstHangulVowel is the first conjoining Hangul vowel jamo.
	firstHangulVowel = 0x1160

	// lastHangulFinal is the last conjoining Hangul final consonant jamo.
	lastHangulFinal = 0x11FF
)

// Length counts the characters a person sees, as the browser's Intl.Segmenter does, so an emoji, flag
// or accented letter counts once. It approximates UAX #29 without its tables, so it can only overcount
// (an Indic conjunct counts as two).
func Length(text string) int {
	count := 0
	started := false
	lastEmoji := false
	joinNext := false
	regionalPending := false
	for _, character := range text {
		if joinNext {
			// A joiner joins only an emoji to the emoji before it; anything else starts its own character.
			joinNext = false
			if unicode.Is(unicode.So, character) {
				continue
			}
		}
		switch {
		case character == zeroWidthJoiner:
			if !started {
				count++
				started = true
			}
			joinNext = lastEmoji
		case isExtending(character):
			if !started {
				count++
				started = true
			}
		case character >= firstRegionalLetter && character <= lastRegionalLetter:
			started, lastEmoji = true, false
			if regionalPending {
				regionalPending = false
				continue
			}
			regionalPending = true
			count++
		default:
			regionalPending = false
			started, lastEmoji = true, unicode.Is(unicode.So, character)
			count++
		}
	}
	return count
}

// isExtending reports whether a code point joins the character before it rather than starting one.
func isExtending(character rune) bool {
	return unicode.In(character, unicode.Mn, unicode.Me, unicode.Mc) ||
		unicode.Is(unicode.Variation_Selector, character) ||
		(character >= firstSkinTone && character <= lastSkinTone) ||
		(character >= firstTag && character <= lastTag) ||
		(character >= firstHangulVowel && character <= lastHangulFinal)
}
