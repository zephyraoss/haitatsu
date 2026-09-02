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
	ctx     context.Context
	session *session
	rawLoad map[string][]byte
}

func (s *session) Search(kind goimapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if s.view == nil {
		return nil, errNotSelected
	}
	ctx := context.Background()
	if err := s.resync(ctx, nil, syncMode{}); err != nil {
		return nil, err
	}
	candidates, err := s.searchCandidates(ctx)
	if err != nil {
		return nil, err
	}
	sc := &searchContext{ctx: ctx, session: s, rawLoad: map[string][]byte{}}
	var seqSet imap.SeqSet
	var uidSet imap.UIDSet
	var count uint32
	var minNum, maxNum uint32
	for _, candidate := range candidates {
		matched, err := sc.matches(criteria, candidate)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
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
	}
	data := &imap.SearchData{Count: count, Min: minNum, Max: maxNum}
	if kind == goimapserver.NumKindUID {
		data.All = uidSet
	} else {
		data.All = seqSet
	}
	return data, nil
}

func (s *session) searchCandidates(ctx context.Context) ([]searchCandidate, error) {
	entries := s.view.entries
	ids := make([]string, 0, len(entries))
	for _, item := range entries {
		if !item.gone {
			ids = append(ids, item.itemID)
		}
	}
	items := map[string]*ent.MailboxMessage{}
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		batch, err := s.client.MailboxMessage.Query().Where(mailboxmessage.IDIn(ids[start:end]...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range batch {
			items[item.ID] = item
		}
	}
	messageIDs := make([]string, 0, len(items))
	for _, item := range items {
		messageIDs = append(messageIDs, item.MessageID)
	}
	msgs := map[string]*ent.Message{}
	for start := 0; start < len(messageIDs); start += 500 {
		end := min(start+500, len(messageIDs))
		batch, err := s.client.Message.Query().Where(message.IDIn(messageIDs[start:end]...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, msg := range batch {
			msgs[msg.ID] = msg
		}
	}
	candidates := make([]searchCandidate, 0, len(entries))
	for index, item := range entries {
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
		candidates = append(candidates, searchCandidate{index: index, entry: item, item: mm, msg: msg})
	}
	return candidates, nil
}

func (sc *searchContext) raw(candidate *searchCandidate) ([]byte, error) {
	if candidate.raw != nil {
		return candidate.raw, nil
	}
	if cached, ok := sc.rawLoad[candidate.msg.ID]; ok {
		candidate.raw = cached
		return cached, nil
	}
	raw, err := sc.session.blobs.GetMessage(sc.ctx, candidate.msg.BlobKey)
	if err != nil {
		return nil, err
	}
	sc.rawLoad[candidate.msg.ID] = raw
	candidate.raw = raw
	return raw, nil
}

func (sc *searchContext) matches(criteria *imap.SearchCriteria, candidate searchCandidate) (bool, error) {
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
		raw, err := sc.raw(&candidate)
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
