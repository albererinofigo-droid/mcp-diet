package prune

import (
	"unicode"
	"unicode/utf8"
)

// Token cost model. A byte-pair encoder splits a word into roughly one token
// per four characters, and merges neighbouring punctuation ("{\"", "\":\"",
// "\"},{\"") into shared tokens at roughly one token per two characters.
const (
	wordCharsPerToken  = 4
	punctCharsPerToken = 2
)

// EstimateTokens approximates how many BPE tokens a JSON payload costs.
//
// It is a heuristic, not a tokenizer: it deliberately avoids shipping a
// vocabulary file, so the binary stays small and the estimate stays free. The
// model is:
//
//   - a run of word characters costs ceil(len/4) tokens
//   - a run of punctuation costs ceil(len/2) tokens, because encoders merge
//     the punctuation clusters that JSON is made of
//   - whitespace ends a word but not a punctuation run, mirroring the way
//     encoders attach a leading space to the token that follows it
//
// Checked against cl100k_base counts in tokens_test.go, this stays inside a
// 0.6x-1.8x band on JSON and prose alike. That is more than accurate enough
// for its only job: reporting a before/after reduction where both sides carry
// the same bias.
func EstimateTokens(b []byte) int {
	tokens, word, punct := 0, 0, 0
	for i := 0; i < len(b); {
		c := b[i]
		var isWord, isSpace bool
		if c < utf8.RuneSelf {
			i++
			isWord, isSpace = wordByte[c], spaceByte[c]
		} else {
			r, size := utf8.DecodeRune(b[i:])
			i += size
			isWord, isSpace = isWordRune(r), unicode.IsSpace(r)
		}

		switch {
		case isWord:
			if punct > 0 {
				tokens += (punct + punctCharsPerToken - 1) / punctCharsPerToken
				punct = 0
			}
			word++
		case isSpace:
			if word > 0 {
				tokens += (word + wordCharsPerToken - 1) / wordCharsPerToken
				word = 0
			}
		default:
			if word > 0 {
				tokens += (word + wordCharsPerToken - 1) / wordCharsPerToken
				word = 0
			}
			punct++
		}
	}
	if word > 0 {
		tokens += (word + wordCharsPerToken - 1) / wordCharsPerToken
	}
	if punct > 0 {
		tokens += (punct + punctCharsPerToken - 1) / punctCharsPerToken
	}
	return tokens
}

// wordByte and spaceByte turn the ASCII classification into a single table
// lookup, which measurably outruns a chain of range comparisons on the
// hundreds of kilobytes a large tools/list carries.
var wordByte, spaceByte = func() ([256]bool, [256]bool) {
	var w, s [256]bool
	for c := 0; c < 256; c++ {
		w[c] = c == '_' ||
			(c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9')
	}
	for _, c := range []byte{' ', '\t', '\n', '\r', '\v', '\f'} {
		s[c] = true
	}
	return w, s
}()

// isWordRune keeps an ASCII fast path: JSON payloads are overwhelmingly ASCII
// and unicode.IsLetter is an order of magnitude slower than a range check.
func isWordRune(r rune) bool {
	if r < utf8.RuneSelf {
		return r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
