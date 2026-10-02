package imapserver

import (
	"context"
	"strings"

	"github.com/emersion/go-imap/v2"
	goimapserver "github.com/emersion/go-imap/v2/imapserver"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailboxmessage"
	"github.com/zephyraoss/haitatsu/internal/database/ent/message"
)

type searchCandidate struct {
	index int
	entry entry
	item  *ent.MailboxMessage
	msg   *ent.Message
	raw   []byte
}

type searchContext struct {
	ctx       context.Context
	session   *session
	scanned   int
	blobBytes int64
}

const (
	searchBatchSize    = 500
	searchMaxMessages  = 100_000
	searchMaxBlobBytes = int64(256) << 20
)

var errSearchTooLarge = &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Search exceeds server limits; narrow the criteria"}

func (s *session) Search(kind goimapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if s.view == nil {
		return nil, errNotSelected
	}
	ctx := context.Background()
	if err := s.resync(ctx, nil, syncMode{}); err != nil {
		return nil, err
	}
	sc := &searchContext{ctx: ctx, session: s}
	var seqSet imap.SeqSet
	var uidSet imap.UIDSet
	var count uint32
	var minNum, maxNum uint32
	err := s.searchCandidates(ctx, func(candidate searchCandidate) error {
		sc.scanned++
		if sc.scanned > searchMaxMessages {
			return errSearchTooLarge
		}
		matched, err := sc.matches(criteria, &candidate)
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}
		count++
		num := uint32(candidate.index + 1)
		if kind == goimapserver.NumKindUID {
			num = candidate.entry.uid
			uidSet.AddNum(imap.UID(num))
		} else {
			seqSet.AddNum(num)
		}
		if minNum == 0 || num < minNum {
			minNum = num
		}
		if num > maxNum {
			maxNum = num
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	data := &imap.SearchData{Count: count, Min: minNum, Max: maxNum}
	if kind == goimapserver.NumKindUID {
		data.All = uidSet
	} else {
		data.All = seqSet
	}
	return data, nil
}

func (s *session) searchCandidates(ctx context.Context, visit func(searchCandidate) error) error {
	entries := s.view.entries
	for start := 0; start < len(entries); start += searchBatchSize {
		end := min(start+searchBatchSize, len(entries))
		ids := make([]string, 0, end-start)
		for _, item := range entries[start:end] {
			if !item.gone {
				ids = append(ids, item.itemID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		itemRows, err := s.client.MailboxMessage.Query().Where(mailboxmessage.IDIn(ids...)).All(ctx)
		if err != nil {
			return err
		}
		items := make(map[string]*ent.MailboxMessage, len(itemRows))
		messageIDs := make([]string, 0, len(itemRows))
		for _, item := range itemRows {
			items[item.ID] = item
			messageIDs = append(messageIDs, item.MessageID)
		}
		msgRows, err := s.client.Message.Query().Where(message.IDIn(messageIDs...)).All(ctx)
		if err != nil {
			return err
		}
		msgs := make(map[string]*ent.Message, len(msgRows))
		for _, msg := range msgRows {
			msgs[msg.ID] = msg
		}
		for offset, item := range entries[start:end] {
			if item.gone {
				continue
			}
			mm, ok := items[item.itemID]
			if !ok {
				continue
			}
			msg, ok := msgs[mm.MessageID]
			if !ok {
				continue
			}
			if err := visit(searchCandidate{index: start + offset, entry: item, item: mm, msg: msg}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (sc *searchContext) raw(candidate *searchCandidate) ([]byte, error) {
	if candidate.raw != nil {
		return candidate.raw, nil
	}
	if sc.blobBytes+candidate.msg.SizeBytes > searchMaxBlobBytes {
		return nil, errSearchTooLarge
	}
	raw, err := sc.session.blobs.GetMessage(sc.ctx, candidate.msg.BlobKey)
	if err != nil {
		return nil, err
	}
	sc.blobBytes += int64(len(raw))
	if sc.blobBytes > searchMaxBlobBytes {
		return nil, errSearchTooLarge
	}
	candidate.raw = raw
	return raw, nil
}

func (sc *searchContext) matches(criteria *imap.SearchCriteria, candidate *searchCandidate) (bool, error) {
	if criteria == nil {
		return true, nil
	}
	seq := uint32(candidate.index + 1)
	for _, set := range criteria.SeqNum {
		if !(&set).Contains(seq) && !(set.Dynamic() && candidate.index == len(sc.session.view.entries)-1) {
			return false, nil
		}
	}
	for _, set := range criteria.UID {
		if !set.Contains(imap.UID(candidate.entry.uid)) && !(set.Dynamic() && candidate.index == len(sc.session.view.entries)-1) {
			return false, nil
		}
	}
	internal := dateOnly(candidate.item.CreatedAt)
	if !criteria.Since.IsZero() && internal.Before(dateOnly(criteria.Since)) {
		return false, nil
	}
	if !criteria.Before.IsZero() && !internal.Before(dateOnly(criteria.Before)) {
		return false, nil
	}
	sent := internal
	if candidate.msg.Date != nil {
		sent = dateOnly(*candidate.msg.Date)
	}
	if !criteria.SentSince.IsZero() && sent.Before(dateOnly(criteria.SentSince)) {
		return false, nil
	}
	if !criteria.SentBefore.IsZero() && !sent.Before(dateOnly(criteria.SentBefore)) {
		return false, nil
	}
	for _, flag := range criteria.Flag {
		if !candidate.entry.flags.Has(string(flag)) {
			return false, nil
		}
	}
	for _, flag := range criteria.NotFlag {
		if candidate.entry.flags.Has(string(flag)) {
			return false, nil
		}
	}
	if criteria.Larger > 0 && candidate.msg.SizeBytes <= criteria.Larger {
		return false, nil
	}
	if criteria.Smaller > 0 && candidate.msg.SizeBytes >= criteria.Smaller {
		return false, nil
	}
	for _, field := range criteria.Header {
		if !headerMatches(candidate.msg, field) {
			return false, nil
		}
	}
	if len(criteria.Body) > 0 || len(criteria.Text) > 0 {
		raw, err := sc.raw(candidate)
		if err != nil {
			return false, err
		}
		body := bodyText(raw)
		for _, term := range criteria.Body {
			if !containsFold(body, term) {
				return false, nil
			}
		}
		for _, term := range criteria.Text {
			if !containsFold(string(raw), term) {
				return false, nil
			}
		}
	}
	for _, not := range criteria.Not {
		matched, err := sc.matches(&not, candidate)
		if err != nil {
			return false, err
		}
		if matched {
			return false, nil
		}
	}
	for _, or := range criteria.Or {
		left, err := sc.matches(&or[0], candidate)
		if err != nil {
			return false, err
		}
		if left {
			continue
		}
		right, err := sc.matches(&or[1], candidate)
		if err != nil {
			return false, err
		}
		if !right {
			return false, nil
		}
	}
	return true, nil
}

func headerMatches(msg *ent.Message, field imap.SearchCriteriaHeaderField) bool {
	key := strings.ToLower(field.Key)
	switch key {
	case "from":
		return anyContains(msg.FromAddresses, field.Value)
	case "to":
		return anyContains(msg.ToAddresses, field.Value)
	case "cc":
		return anyContains(msg.CcAddresses, field.Value)
	case "bcc":
		return anyContains(msg.BccAddresses, field.Value)
	case "subject":
		return containsFold(msg.Subject, field.Value)
	}
	for name, values := range msg.Headers {
		if strings.ToLower(name) != key {
			continue
		}
		if field.Value == "" {
			return true
		}
		if anyContains(values, field.Value) {
			return true
		}
	}
	return false
}

func anyContains(values []string, needle string) bool {
	if needle == "" {
		return len(values) > 0
	}
	for _, value := range values {
		if containsFold(value, needle) {
			return true
		}
	}
	return false
}

func bodyText(raw []byte) string {
	_, body, ok := strings.Cut(string(raw), "\r\n\r\n")
	if !ok {
		_, body, _ = strings.Cut(string(raw), "\n\n")
	}
	return body
}
