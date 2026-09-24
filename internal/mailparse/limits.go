package mailparse

import (
	"container/list"
	"errors"
	"io"
	"strings"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

// Bounds applied to every walk over untrusted MIME so a single message cannot
// force unbounded allocation or CPU work.
const (
	MaxParts       = 256
	MaxDepth       = 8
	MaxAttachments = 64
	MaxHeaderBytes = 32 * 1024
	MaxStatusBytes = 256 * 1024
)

var (
	ErrTooManyParts = errors.New("mailparse: too many MIME parts")
	ErrTooDeep      = errors.New("mailparse: MIME nesting too deep")
)

type multipartLevel struct {
	reader message.MultipartReader
	depth  int
}

// boundedReader flattens a MIME tree into leaf parts like mail.Reader, but
// stops once MaxParts entities have been visited or nesting exceeds MaxDepth.
type boundedReader struct {
	Header  mail.Header
	levels  *list.List
	visited int
}

func newBoundedReader(r io.Reader) (*boundedReader, error) {
	entity, err := message.ReadWithOptions(r, &message.ReadOptions{MaxHeaderBytes: MaxHeaderBytes})
	if entity == nil {
		return nil, err
	}
	if err != nil && !message.IsUnknownCharset(err) {
		return nil, err
	}
	mr := entity.MultipartReader()
	if mr == nil {
		var h message.Header
		h.Set("Content-Type", "multipart/mixed")
		me, _ := message.NewMultipart(h, []*message.Entity{entity})
		mr = me.MultipartReader()
	}
	levels := list.New()
	levels.PushBack(multipartLevel{reader: mr, depth: 0})
	return &boundedReader{Header: mail.Header{Header: entity.Header}, levels: levels}, err
}

func (r *boundedReader) NextPart() (*mail.Part, error) {
	for r.levels.Len() > 0 {
		element := r.levels.Back()
		level := element.Value.(multipartLevel)

		if r.visited >= MaxParts {
			return nil, ErrTooManyParts
		}
		p, err := level.reader.NextPart()
		if err == io.EOF {
			r.levels.Remove(element)
			continue
		} else if err != nil && !message.IsUnknownCharset(err) {
			return nil, err
		}
		r.visited++

		if pmr := p.MultipartReader(); pmr != nil {
			if level.depth+1 > MaxDepth {
				return nil, ErrTooDeep
			}
			r.levels.PushBack(multipartLevel{reader: pmr, depth: level.depth + 1})
			continue
		}
		part := &mail.Part{Body: p.Body}
		t, _, _ := p.Header.ContentType()
		disp, _, _ := p.Header.ContentDisposition()
		if disp == "inline" || (disp != "attachment" && strings.HasPrefix(t, "text/")) {
			part.Header = &mail.InlineHeader{Header: p.Header}
		} else {
			part.Header = &mail.AttachmentHeader{Header: p.Header}
		}
		return part, err
	}
	return nil, io.EOF
}

func (r *boundedReader) Close() error {
	for r.levels.Len() > 0 {
		element := r.levels.Back()
		if err := element.Value.(multipartLevel).reader.Close(); err != nil {
			return err
		}
		r.levels.Remove(element)
	}
	return nil
}
