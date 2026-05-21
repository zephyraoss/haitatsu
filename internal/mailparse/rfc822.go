package mailparse

import "bytes"

func NormalizeMessage(raw []byte) []byte {
	header, body := SplitHeaderBody(raw)
	return JoinHeaderBody(header, body)
}

func SplitHeaderBody(raw []byte) (header, body []byte) {
	if index := bytes.Index(raw, []byte("\r\n\r\n")); index >= 0 {
		return raw[:index], raw[index+4:]
	}
	if index := bytes.Index(raw, []byte("\n\n")); index >= 0 {
		return raw[:index], raw[index+2:]
	}
	return splitHeaderBodyLines(raw)
}

func JoinHeaderBody(header, body []byte) []byte {
	header = toCRLF(header)
	if len(body) == 0 {
		return ensureHeaderBodySeparator(header)
	}
	body = toCRLF(body)
	if len(header) == 0 {
		return body
	}
	return append(ensureHeaderBodySeparator(header), body...)
}

func ensureHeaderBodySeparator(header []byte) []byte {
	if bytes.HasSuffix(header, []byte("\r\n\r\n")) {
		return header
	}
	if bytes.HasSuffix(header, []byte("\r\n")) {
		return append(header, []byte("\r\n")...)
	}
	return append(header, []byte("\r\n\r\n")...)
}

func splitHeaderBodyLines(raw []byte) (header, body []byte) {
	normalized := bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n"))
	lines := bytes.Split(normalized, []byte("\n"))
	var headerLines [][]byte
	var bodyLines [][]byte
	inBody := false
	for _, line := range lines {
		if inBody {
			bodyLines = append(bodyLines, line)
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			inBody = true
			continue
		}
		if !isHeaderLine(line) && len(headerLines) > 0 {
			inBody = true
			bodyLines = append(bodyLines, line)
			continue
		}
		headerLines = append(headerLines, line)
	}
	return joinLines(headerLines), joinLines(bodyLines)
}

func joinLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for index, line := range lines {
		if index > 0 {
			buf.WriteByte('\n')
		}
		buf.Write(line)
	}
	return buf.Bytes()
}

func toCRLF(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	normalized := bytes.ReplaceAll(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n"))
	lines := bytes.Split(normalized, []byte("\n"))
	var buf bytes.Buffer
	for index, line := range lines {
		if index > 0 {
			buf.WriteString("\r\n")
		}
		buf.Write(bytes.TrimRight(line, "\r"))
	}
	if len(buf.Bytes()) > 0 {
		buf.WriteString("\r\n")
	}
	return buf.Bytes()
}

func isHeaderLine(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == ' ' || trimmed[0] == '\t' {
		return true
	}
	colon := bytes.IndexByte(trimmed, ':')
	if colon <= 0 {
		return false
	}
	for _, b := range trimmed[:colon] {
		if !isHeaderKeyByte(b) {
			return false
		}
	}
	return true
}

func isHeaderKeyByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	default:
		return b == '-'
	}
}
