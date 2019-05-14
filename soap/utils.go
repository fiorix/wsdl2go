package soap

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

var removeNonUTF = func(r rune) rune {
	if r == utf8.RuneError {
		return -1
	}
	return r
}

// RemoveNonUTF8Strings removes strings that isn't UTF-8 encoded
func RemoveNonUTF8Strings(string string) string {
	return strings.Map(removeNonUTF, string)
}

// RemoveNonUTF8Bytes removes bytes that isn't UTF-8 encoded
func RemoveNonUTF8Bytes(data []byte) []byte {
	return bytes.Map(removeNonUTF, data)
}
