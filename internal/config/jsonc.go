package config

// stripJSONComments removes // and /* */ comments while leaving string
// literals untouched, so URLs such as "https://example.com" survive.
func stripJSONComments(text string) string {
	var out []byte
	inString := false
	inLine := false
	inBlock := false
	for index := 0; index < len(text); index++ {
		char := text[index]
		var next byte
		if index+1 < len(text) {
			next = text[index+1]
		}
		if inLine {
			if char == '\n' {
				inLine = false
				out = append(out, char)
			}
			continue
		}
		if inBlock {
			if char == '*' && next == '/' {
				inBlock = false
				index++
			}
			continue
		}
		if inString {
			out = append(out, char)
			if char == '\\' && index+1 < len(text) {
				out = append(out, text[index+1])
				index++
			} else if char == '"' {
				inString = false
			}
			continue
		}
		if char == '"' {
			inString = true
			out = append(out, char)
			continue
		}
		if char == '/' && next == '/' {
			inLine = true
			index++
			continue
		}
		if char == '/' && next == '*' {
			inBlock = true
			index++
			continue
		}
		out = append(out, char)
	}
	return string(out)
}

// dropTrailingCommas removes a comma that is followed only by whitespace and
// the closing bracket of an object or array.
func dropTrailingCommas(text string) string {
	var out []byte
	inString := false
	pendingComma := -1
	for index := 0; index < len(text); index++ {
		char := text[index]
		if inString {
			if char == '\\' && index+1 < len(text) {
				out = append(out, char, text[index+1])
				index++
				continue
			}
			out = append(out, char)
			if char == '"' {
				inString = false
			}
			continue
		}
		if char == '"' {
			inString = true
			pendingComma = -1
			out = append(out, char)
			continue
		}
		if char == ',' {
			pendingComma = len(out)
			out = append(out, char)
			continue
		}
		if (char == '}' || char == ']') && pendingComma >= 0 {
			out = append(out[:pendingComma], out[pendingComma+1:]...)
			pendingComma = -1
		} else if char != ' ' && char != '\t' && char != '\n' && char != '\r' {
			pendingComma = -1
		}
		out = append(out, char)
	}
	return string(out)
}
