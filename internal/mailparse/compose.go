package mailparse

import (
	"bytes"
	"fmt"
	"io"
	"net/mail"
	"net/textproto"
	"sort"
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
	body = StripHaitatsuHeadersFromBody(body)
	h := textproto.MIMEHeader(msg.Header)
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
			fmt.Fprintf(&buf, "%s: %s\r\n", key, value)
		}
	}
	buf.WriteString("\r\n")
	buf.Write(body)
	return buf.Bytes()
}

func StripHaitatsuHeadersFromBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var out bytes.Buffer
	for len(body) > 0 {
		line, rest, hasNext := bytes.Cut(body, []byte("\n"))
		if !hasNext {
			rest = nil
		}
		trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if len(trimmed) > 0 && bytes.HasPrefix(bytes.ToLower(trimmed), []byte("x-haitatsu-")) {
			body = rest
			continue
		}
		out.Write(line)
		if hasNext {
			out.WriteByte('\n')
		}
		body = rest
	}
	return out.Bytes()
}
