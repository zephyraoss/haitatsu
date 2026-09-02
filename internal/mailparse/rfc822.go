package mailparse

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

func NormalizeMessage(raw []byte) []byte {
	header, body := SplitHeaderBody(raw)
	return JoinHeaderBody(header, body)
}

func SplitHeaderBody(raw []byte) (header, body []byte) {
	if header, body, ok := splitHeaderBodyRFC822(raw); ok {
		return header, body
	}
	if header, body, ok := splitAtSeparator(raw, []byte("\r\n\n"), 2, 3); ok {
		return header, body
	}
	for search := 0; ; {
		segment := raw[search:]
		index := bytes.Index(segment, []byte("\r\n\r\n"))
		if index < 0 {
			break
		}
		index += search
		header, body = raw[:index], raw[index+4:]
		if headerBlockValid(header) {
			return header, body
		}
		search = index + 4
	}
	if index := indexLFHeaderEnd(raw); index >= 0 {
		return raw[:index], raw[index+2:]
	}
	return splitHeaderBodyLines(raw)
}

func splitHeaderBodyRFC822(raw []byte) (header, body []byte, ok bool) {
	reader := bufio.NewReader(bytes.NewReader(raw))
	var headerBuf bytes.Buffer
	foundEnd := false
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if isHeaderTerminatorLine(line) && headerBuf.Len() > 0 {
				foundEnd = true
				break
			}
			headerBuf.Write(line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, false
		}
	}
	if !foundEnd {
		return nil, nil, false
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, false
	}
	return headerBuf.Bytes(), rest, true
}

func isHeaderTerminatorLine(line []byte) bool {
	return len(bytes.TrimSpace(bytes.TrimRight(line, "\r\n"))) == 0
}

func splitAtSeparator(raw, separator []byte, headerEnd, bodyStart int) (header, body []byte, ok bool) {
	index := bytes.Index(raw, separator)
	if index < 0 {
		return nil, nil, false
	}
	header, body = raw[:index+headerEnd], raw[index+bodyStart:]
	if !headerBlockValid(header) {
		return nil, nil, false
	}
	return header, body, true
}

func headerBlockValid(header []byte) bool {
	normalized := bytes.ReplaceAll(bytes.ReplaceAll(header, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n"))
	for _, line := range bytes.Split(normalized, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !isHeaderLine(line) {
			return false
		}
	}
	return true
}

func indexLFHeaderEnd(raw []byte) int {
	for index := 0; index+1 < len(raw); index++ {
		if raw[index] != '\n' || raw[index+1] != '\n' {
			continue
		}
		if index > 0 && raw[index-1] == '\r' {
			continue
		}
		if headerBlockValid(raw[:index]) {
			return index
		}
	}
	return -1
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
	header = joinLines(headerLines)
	if len(header) > 0 {
		header = append(header, '\n')
	}
	return header, joinLines(bodyLines)
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

func AppendHeaderFields(header []byte, fields ...[]byte) []byte {
	header = bytes.TrimRight(header, "\r\n")
	for _, field := range fields {
		if len(header) > 0 {
			header = append(header, '\n')
		}
		header = append(header, bytes.TrimRight(field, "\r\n")...)
	}
	return header
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

func RemoveHeaderField(header []byte, key string) []byte {
	prefix := []byte(strings.ToLower(key) + ":")
	lines := bytes.Split(header, []byte("\n"))
	kept := make([][]byte, 0, len(lines))
	skipping := false
	for _, line := range lines {
		trimmed := bytes.TrimRight(line, "\r")
		if len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t') {
			if skipping {
				continue
			}
			kept = append(kept, line)
			continue
		}
		skipping = bytes.HasPrefix(bytes.ToLower(trimmed), prefix)
		if skipping {
			continue
		}
		kept = append(kept, line)
	}
	return bytes.Join(kept, []byte("\n"))
}
