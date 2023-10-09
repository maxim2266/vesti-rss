package xmlutil

import "unicode/utf8"

// AppendEscaped appends the given text to the given byte slice, with XML entities escaped
func AppendEscaped(dest []byte, text string) []byte {
	var esc string

	last := 0

	for i := 0; i < len(text); {
		r, width := utf8.DecodeRuneInString(text[i:])

		switch r {
		case '"':
			esc = "&quot;"
		case '\'':
			esc = "&apos;"
		case '&':
			esc = "&amp;"
		case '<':
			esc = "&lt;"
		case '>':
			esc = "&gt;"
		default:
			if isValidXmlChar(r) && (r != utf8.RuneError || width != 1) {
				i += width
				continue
			}

			esc = `\uFFFD`
		}

		dest = append(append(dest, text[last:i]...), esc...)
		i += width
		last = i
	}

	return append(dest, text[last:]...)
}

// AppendTag appends the text eclosed in the tag to the byte slice, with XML entities escaped
func AppendTag(dest []byte, tag, text string) []byte {
	dest = append(append(append(dest, '<'), tag...), '>')
	dest = AppendEscaped(dest, text)

	return append(append(append(dest, "</"...), tag...), '>')
}

// check if the rune is a valid XML character, as in section 2.2 of https://www.xml.com/axml/testaxml.htm
func isValidXmlChar(r rune) bool {
	return r == 0x09 ||
		r == 0x0A ||
		r == 0x0D ||
		r >= 0x20 && r <= 0xD7FF ||
		r >= 0xE000 && r <= 0xFFFD ||
		r >= 0x10000 && r <= 0x10FFFF
}
