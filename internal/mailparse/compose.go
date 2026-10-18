package mailparse

import (
	"bytes"
	"io"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"
)

func InjectHeaders(raw []byte, mutate func(textproto.MIMEHeader)) ([]byte, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return nil, err
	}
	h := textproto.MIMEHeader(msg.Header)
	for key := range h {
		if strings.HasPrefix(strings.ToLower(key), "x-haitatsu-") {
			delete(h, key)
		}
	}
	mutate(h)
	return FormatRFC822(h, body), nil
}

func FormatRFC822(h textproto.MIMEHeader, body []byte) []byte {
	keys := make([]string, 0, len(h))
	for key := range h {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	for _, key := range keys {
		for _, value := range h[key] {
			buf.WriteString(foldHeaderLine(key, value))
			buf.WriteString("\r\n")
		}
	}
	buf.WriteString("\r\n")
	buf.Write(body)
	return buf.Bytes()
}

func foldHeaderLine(key string, value string) string {
	line := key + ": " + value
	minimum := len(key) + 2
	var b strings.Builder
	for len(line) > 78 {
		i := strings.LastIndexAny(line[:78], " \t")
		if i < minimum {
			i = strings.IndexAny(line[minimum:], " \t")
			if i < 0 {
				break
			}
			i += minimum
		}
		b.WriteString(line[:i])
		b.WriteString("\r\n")
		line = line[i:]
		minimum = len(line) - len(strings.TrimLeft(line, " \t")) + 1
	}
	b.WriteString(line)
	return b.String()
}

func StripHaitatsuHeaders(header []byte) []byte {
	lines := bytes.Split(header, []byte("\n"))
	kept := make([][]byte, 0, len(lines))
	skipping := false
	for _, line := range lines {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if !skipping {
				kept = append(kept, line)
			}
			continue
		}
		skipping = bytes.HasPrefix(bytes.ToLower(line), []byte("x-haitatsu-"))
		if !skipping {
			kept = append(kept, line)
		}
	}
	return bytes.Join(kept, []byte("\n"))
}
