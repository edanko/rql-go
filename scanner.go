package rql

import "strings"

// scanner is a byte-level lexer over the raw query string.
//
// It never allocates and never materialises token text: every token is a byte
// range into the source. Percent escapes are validated here (so that malformed
// input fails before the parser) but decoded lazily by the parser, only for the
// tokens that need a string.
type scanner struct {
	src       string
	pos       int
	maxTokens int
	tokens    int
}

// newScanner prepares a scanner over src. maxTokens caps the number of tokens;
// a non-positive value disables the cap (the Config always supplies one).
func newScanner(src string, maxTokens int) scanner {
	return scanner{src: src, maxTokens: maxTokens}
}

// Single-byte tokens that map straight to one kind.
const (
	bTab   = '\t'
	bLF    = '\n'
	bCR    = '\r'
	bSpc   = ' '
	bPct   = '%'
	bPlus  = '+'
	bLpar  = '('
	bRpar  = ')'
	bComma = ','
	bAmp   = '&'
	bPipe  = '|'
	bEq    = '='
	bLt    = '<'
	bGt    = '>'
	bBang  = '!'
	bZero  = '0'
	bNine  = '9'
	bA     = 'a'
	bF     = 'f'
	bBigA  = 'A'
	bBigF  = 'F'
	bDel   = 0x7f
)

// isDelim reports whether c terminates a word token.
func isDelim(c byte) bool {
	switch c {
	case bLpar, bRpar, bComma, bAmp, bPipe, bEq, bLt, bGt, bBang:
		return true
	default:
		return false
	}
}

// isSpaceByte reports whether c is insignificant whitespace.
func isSpaceByte(c byte) bool {
	return c == bSpc || c == bTab || c == bLF || c == bCR
}

// isHexDigit reports whether c is a hexadecimal digit.
func isHexDigit(c byte) bool {
	switch {
	case c >= bZero && c <= bNine:
		return true
	case c >= bA && c <= bF:
		return true
	case c >= bBigA && c <= bBigF:
		return true
	default:
		return false
	}
}

// hexValue returns the value of a hex digit, or -1 when c is not one.
func hexValue(c byte) int {
	switch {
	case c >= bZero && c <= bNine:
		return int(c - bZero)
	case c >= bA && c <= bF:
		return int(c-bA) + 10
	case c >= bBigA && c <= bBigF:
		return int(c-bBigA) + 10
	default:
		return -1
	}
}

// skipSpace advances past whitespace and rejects control characters.
func (s *scanner) skipSpace() *Error {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		if isSpaceByte(c) {
			s.pos++
			continue
		}
		if c < 0x20 || c == bDel {
			return errAt(ErrInvalidSyntax, s.pos, "control character in query")
		}
		return nil
	}
	return nil
}

// next returns the next token, or a diagnostic error on malformed input.
func (s *scanner) next() (Token, *Error) {
	if err := s.skipSpace(); err != nil {
		return Token{}, err
	}
	if s.pos >= len(s.src) {
		return Token{Kind: tokEOF, Start: len(s.src), End: len(s.src)}, nil
	}
	s.tokens++
	if s.maxTokens > 0 && s.tokens > s.maxTokens {
		return Token{}, errAt(ErrTooComplex, s.pos, "too many tokens")
	}
	start := s.pos
	if s.src[s.pos] == bBang {
		s.pos++
		if s.pos < len(s.src) && s.src[s.pos] == bEq {
			s.pos++
			return Token{Kind: tokNe, Start: start, End: s.pos}, nil
		}
		return Token{}, errAt(ErrInvalidSyntax, start, "unexpected '!'")
	}
	if k, ok := punctuation(s.src[s.pos]); ok {
		s.pos++
		return s.longestMatch(k, start), nil
	}
	return s.word(start)
}

// punctuation maps a delimiting byte to its single-byte token kind.
func punctuation(c byte) (tokKind, bool) {
	switch c {
	case bLpar:
		return tokLParen, true
	case bRpar:
		return tokRParen, true
	case bComma:
		return tokComma, true
	case bAmp:
		return tokAnd, true
	case bPipe:
		return tokOr, true
	case bEq:
		return tokEq, true
	case bLt:
		return tokLt, true
	case bGt:
		return tokGt, true
	case bBang:
		return tokNe, true
	default:
		return tokEOF, false
	}
}

// longestMatch extends '<', '>' and '!' to their two-byte forms.
func (s *scanner) longestMatch(k tokKind, start int) Token {
	if s.pos < len(s.src) && s.src[s.pos] == bEq {
		switch k {
		case tokLt:
			s.pos++
			return Token{Kind: tokLe, Start: start, End: s.pos}
		case tokGt:
			s.pos++
			return Token{Kind: tokGe, Start: start, End: s.pos}
		case tokNe:
			s.pos++
			return Token{Kind: tokNe, Start: start, End: s.pos}
		default:
			return Token{Kind: k, Start: start, End: s.pos}
		}
	}
	return Token{Kind: k, Start: start, End: s.pos}
}

// word scans a run of non-delimiter, non-space bytes, validating percent escapes.
func (s *scanner) word(start int) (Token, *Error) {
	var flags uint8
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		if isDelim(c) || isSpaceByte(c) {
			break
		}
		if c < 0x20 || c == bDel {
			return Token{}, errAt(ErrInvalidSyntax, s.pos, "control character in query")
		}
		if c == bPct {
			if s.pos+2 >= len(s.src) || !isHexDigit(s.src[s.pos+1]) || !isHexDigit(s.src[s.pos+2]) {
				return Token{}, errAt(ErrInvalidSyntax, s.pos, "invalid percent escape")
			}
			flags |= tokHasEscape
			s.pos += 3
			continue
		}
		if c == bPlus {
			flags |= tokHasPlus
		}
		s.pos++
	}
	return Token{Kind: tokWord, Start: start, End: s.pos, flags: flags}, nil
}

// decode returns the decoded text of a word token. When the token contains no
// percent escapes (and no '+' to translate) the result is a zero-copy slice of
// the source; otherwise it is freshly allocated.
//
// plusAsSpace decodes '+' as ' ' for URL interop; it is enabled for values and
// disabled for identifiers so that sort(+name) keeps its direction prefix.
func decode(src string, t Token, plusAsSpace bool) string {
	raw := src[t.Start:t.End]
	if t.flags == 0 {
		return raw
	}
	if t.flags == tokHasPlus && !plusAsSpace {
		return raw
	}
	// A strings.Builder is used rather than a byte slice because its String
	// method hands out the buffer without a copy, halving the allocation cost.
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == bPct:
			// The scanner validated that two hex digits follow, so this index
			// arithmetic is in range and the value fits in a byte.
			hi := hexValue(raw[i+1])
			lo := hexValue(raw[i+2])
			b.WriteByte(byte(hi<<4 | lo)) // #nosec G115 -- bounded by isHexDigit (0..15).
			i += 2
		case c == bPlus && plusAsSpace:
			b.WriteByte(bSpc)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
