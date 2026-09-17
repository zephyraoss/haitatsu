package spam

import (
	"bytes"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
)

const TypeSafeExtractorVersion = "1"
const (
	typeSafeMaxParts         = 32
	typeSafeMaxLinks         = 40
	typeSafeMaxLinkBytes     = 512
	typeSafeMaxHeaderBytes   = 1024
	typeSafeMaxAttachments   = 20
	typeSafeMaxAttachmentLen = 200
	typeSafeMaxScanBytes     = 2 * 1024 * 1024
	typeSafeMaxPartText      = 64 * 1024
)

type TypeSafeInputInfo struct {
	Status     string `json:"status"`
	Truncated  bool   `json:"truncated"`
	Bytes      int    `json:"bytes"`
	PlainBytes int    `json:"plain_bytes"`
	HTMLBytes  int    `json:"html_bytes"`
	Parts      int    `json:"parts"`
	Links      int    `json:"links"`
	ParseError string `json:"parse_error,omitempty"`
}

type typeSafeLink struct {
	Label       string `json:"label"`
	Destination string `json:"destination"`
}

// BuildTypeSafeState extracts only bounded message evidence. It never resolves
// destinations or reads attachment contents into the state.
func BuildTypeSafeState(raw []byte, maxTextBytes int, authResults map[string]any) (map[string]any, TypeSafeInputInfo) {
	if maxTextBytes <= 0 {
		maxTextBytes = 16384
	}
	if maxTextBytes > typeSafeMaxPartText {
		maxTextBytes = typeSafeMaxPartText
	}
	info := TypeSafeInputInfo{Status: "ok"}
	if len(raw) > typeSafeMaxScanBytes {
		raw = raw[:typeSafeMaxScanBytes]
		info.Truncated = true
	}
	state := map[string]any{}
	entity, err := message.ReadWithOptions(bytes.NewReader(raw), &message.ReadOptions{MaxHeaderBytes: 32 * 1024})
	if entity == nil {
		info.Status = "skipped_parse_error"
		info.ParseError = "invalid_or_oversized_headers"
		return state, info
	}
	if err != nil {
		info.ParseError = "unsupported_encoding"
	}
	header := mail.Header{Header: entity.Header}
	bounded := func(text string, limit int) string {
		value, cut := clampUTF8(text, limit)
		info.Truncated = info.Truncated || cut
		return value
	}
	headerText := func(key string) string {
		value, e := header.Text(key)
		if e != nil {
			info.ParseError = "invalid_header"
		}
		return bounded(value, typeSafeMaxHeaderBytes)
	}
	addresses := func(key string) []map[string]string {
		entries, e := header.AddressList(key)
		if e != nil && header.Get(key) != "" {
			info.ParseError = "invalid_address"
		}
		if len(entries) > 5 {
			entries = entries[:5]
			info.Truncated = true
		}
		out := make([]map[string]string, 0, len(entries))
		for _, entry := range entries {
			out = append(out, map[string]string{"name": bounded(entry.Name, typeSafeMaxHeaderBytes), "address": bounded(strings.ToLower(entry.Address), typeSafeMaxHeaderBytes)})
		}
		return out
	}
	email := map[string]any{"subject": headerText("Subject"), "from": addresses("From"), "reply_to": addresses("Reply-To")}
	var plain, htmlSource string
	var attachments []map[string]string
	var walk func(*message.Entity, int)
	walk = func(part *message.Entity, depth int) {
		if info.Parts >= typeSafeMaxParts {
			info.Truncated = true
			return
		}
		info.Parts++
		if depth > 8 {
			info.Truncated = true
			return
		}
		kind, _, e := part.Header.ContentType()
		if e != nil {
			info.ParseError = "invalid_content_type"
			return
		}
		disposition, params, _ := part.Header.ContentDisposition()
		if disposition == "attachment" || (!strings.HasPrefix(kind, "text/") && !strings.HasPrefix(kind, "multipart/")) {
			if len(attachments) >= typeSafeMaxAttachments {
				info.Truncated = true
				return
			}
			filename := params["filename"]
			if filename == "" {
				_, p, _ := part.Header.ContentType()
				filename = p["name"]
			}
			attachments = append(attachments, map[string]string{"filename": bounded(filename, typeSafeMaxAttachmentLen), "content_type": bounded(kind, 100)})
			return
		}
		if mr := part.MultipartReader(); mr != nil {
			for {
				if info.Parts >= typeSafeMaxParts {
					info.Truncated = true
					return
				}
				next, e := mr.NextPart()
				if e == io.EOF {
					return
				}
				if next == nil {
					info.ParseError = "invalid_mime_part"
					return
				}
				if e != nil {
					info.ParseError = "unsupported_encoding"
				}
				walk(next, depth+1)
			}
		}
		if kind != "text/plain" && kind != "text/html" {
			return
		}
		if (kind == "text/plain" && plain != "") || (kind == "text/html" && htmlSource != "") {
			info.Truncated = true
			return
		}
		data, e := io.ReadAll(io.LimitReader(part.Body, typeSafeMaxPartText+1))
		if e != nil {
			info.ParseError = "invalid_body_encoding"
		}
		text := bounded(string(data), typeSafeMaxPartText)
		if kind == "text/plain" {
			plain = text
		} else {
			htmlSource = text
		}
	}
	walk(entity, 0)
	htmlText, links := htmlEvidence(htmlSource)
	plain = sanitizeBodyURLs(normalizeText(plain))
	htmlText = sanitizeBodyURLs(htmlText)
	for _, candidate := range typeSafeURLPattern.FindAllString(plain, -1) {
		if destination := linkEvidence(candidate); destination != "" {
			links = append(links, typeSafeLink{Label: clamp(candidate, typeSafeMaxLinkBytes), Destination: destination})
		}
	}
	if len(links) > typeSafeMaxLinks {
		links = links[:typeSafeMaxLinks]
		info.Truncated = true
	}
	// The alternatives share one final text budget, independently of MIME order.
	if plain == htmlText {
		htmlText = ""
	}
	plainBudget, htmlBudget := maxTextBytes, 0
	if plain == "" {
		plainBudget, htmlBudget = 0, maxTextBytes
	} else if htmlText != "" {
		htmlBudget = maxTextBytes / 4
		if htmlBudget > len(htmlText) {
			htmlBudget = len(htmlText)
		}
		plainBudget = maxTextBytes - htmlBudget
		if len(plain) < plainBudget {
			htmlBudget += plainBudget - len(plain)
			plainBudget = len(plain)
		}
	}
	plain = bounded(plain, plainBudget)
	htmlText = bounded(htmlText, htmlBudget)
	info.PlainBytes, info.HTMLBytes = len(plain), len(htmlText)
	info.Bytes = info.PlainBytes + info.HTMLBytes
	info.Links = len(links)
	if strings.TrimSpace(plain) == "" && strings.TrimSpace(htmlText) == "" && len(links) == 0 {
		info.Status = "skipped_no_content"
		if info.ParseError != "" {
			info.Status = "skipped_parse_error"
		}
	} else if info.Truncated || info.ParseError != "" {
		info.Status = "partial"
	}
	email["plain_text"], email["html_text"], email["links"], email["attachments"] = plain, htmlText, links, attachments
	state["email"] = email
	state["truncated"], state["parse_status"] = info.Truncated, info.Status
	auth := make(map[string]string)
	for _, key := range []string{"spf", "spf_domain", "dkim", "dkim_domain", "dmarc", "dmarc_policy"} {
		if value, ok := authResults[key].(string); ok {
			auth[key] = clamp(value, typeSafeMaxHeaderBytes)
		}
	}
	state["authentication"] = auth
	return state, info
}

var typeSafeURLPattern = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)

func sanitizeBodyURLs(body string) string {
	return typeSafeURLPattern.ReplaceAllStringFunc(body, func(candidate string) string {
		if clean := linkEvidence(candidate); clean != "" {
			return clean
		}
		return "[invalid URL]"
	})
}

func htmlEvidence(source string) (string, []typeSafeLink) {
	tokenizer := html.NewTokenizer(strings.NewReader(source))
	var text, label strings.Builder
	var links []typeSafeLink
	skip := false
	href := ""
	flushLink := func() {
		if href != "" && len(links) <= typeSafeMaxLinks {
			links = append(links, typeSafeLink{Label: clamp(sanitizeBodyURLs(normalizeText(label.String())), typeSafeMaxLinkBytes), Destination: href})
		}
		href = ""
		label.Reset()
	}
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			flushLink()
			return normalizeText(text.String()), links
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data == "script" || token.Data == "style" {
				skip = true
				continue
			}
			if skip {
				continue
			}
			if token.Data == "a" {
				flushLink()
				for _, attr := range token.Attr {
					if attr.Key == "href" {
						href = linkEvidence(attr.Val)
					}
				}
			}
			text.WriteByte(' ')
		case html.EndTagToken:
			token := tokenizer.Token()
			if token.Data == "script" || token.Data == "style" {
				skip = false
				continue
			}
			if skip {
				continue
			}
			if token.Data == "a" {
				flushLink()
			}
			text.WriteByte(' ')
		case html.TextToken:
			if skip {
				continue
			}
			value := tokenizer.Text()
			text.Write(value)
			if href != "" {
				label.Write(value)
			}
		}
	}
}

func linkEvidence(href string) string {
	href = strings.TrimSpace(href)
	parsed, err := url.Parse(href)
	if err != nil || parsed.Opaque != "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	if parsed.Host == "" && (parsed.Scheme != "" || parsed.Path == "") {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	parsed.Host = strings.ToLower(parsed.Host)
	return clamp(parsed.String(), typeSafeMaxLinkBytes)
}

func normalizeText(text string) string {
	return strings.Join(strings.Fields(strings.ToValidUTF8(text, "�")), " ")
}
func clampUTF8(text string, limit int) (string, bool) {
	valid := strings.ToValidUTF8(text, "�")
	changed := valid != text
	if limit < 0 {
		limit = 0
	}
	if len(valid) <= limit {
		return valid, changed
	}
	valid = valid[:limit]
	for len(valid) > 0 && !utf8.ValidString(valid) {
		valid = valid[:len(valid)-1]
	}
	return valid, true
}
func clamp(text string, limit int) string { value, _ := clampUTF8(text, limit); return value }
