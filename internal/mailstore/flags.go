package mailstore

import (
	"slices"
	"strings"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
)

const (
	FlagSeen     = "\\Seen"
	FlagAnswered = "\\Answered"
	FlagFlagged  = "\\Flagged"
	FlagDeleted  = "\\Deleted"
	FlagDraft    = "\\Draft"
)

type Flags struct {
	Seen     bool
	Answered bool
	Flagged  bool
	Deleted  bool
	Draft    bool
	Keywords []string
}

func FlagsOf(item *ent.MailboxMessage) Flags {
	return Flags{Seen: item.Read, Answered: item.Answered, Flagged: item.Flagged, Deleted: item.ImapDeleted, Draft: item.Draft, Keywords: slices.Clone(item.Keywords)}
}

func (f Flags) List() []string {
	flags := make([]string, 0, 5+len(f.Keywords))
	if f.Seen {
		flags = append(flags, FlagSeen)
	}
	if f.Answered {
		flags = append(flags, FlagAnswered)
	}
	if f.Flagged {
		flags = append(flags, FlagFlagged)
	}
	if f.Deleted {
		flags = append(flags, FlagDeleted)
	}
	if f.Draft {
		flags = append(flags, FlagDraft)
	}
	return append(flags, f.Keywords...)
}

func ParseFlags(values []string) Flags {
	var flags Flags
	for _, value := range values {
		flags.Set(value, true)
	}
	return flags
}

func (f *Flags) Set(flag string, value bool) {
	switch strings.ToLower(flag) {
	case "\\seen":
		f.Seen = value
	case "\\answered":
		f.Answered = value
	case "\\flagged":
		f.Flagged = value
	case "\\deleted":
		f.Deleted = value
	case "\\draft":
		f.Draft = value
	case "\\recent", "\\*":
	default:
		if value {
			if !slices.ContainsFunc(f.Keywords, func(k string) bool { return strings.EqualFold(k, flag) }) {
				f.Keywords = append(f.Keywords, flag)
			}
			return
		}
		f.Keywords = slices.DeleteFunc(f.Keywords, func(k string) bool { return strings.EqualFold(k, flag) })
	}
}

func (f Flags) Has(flag string) bool {
	switch strings.ToLower(flag) {
	case "\\seen":
		return f.Seen
	case "\\answered":
		return f.Answered
	case "\\flagged":
		return f.Flagged
	case "\\deleted":
		return f.Deleted
	case "\\draft":
		return f.Draft
	default:
		return slices.ContainsFunc(f.Keywords, func(k string) bool { return strings.EqualFold(k, flag) })
	}
}

func applyFlags(update *ent.MailboxMessageUpdateOne, flags Flags) *ent.MailboxMessageUpdateOne {
	keywords := flags.Keywords
	if keywords == nil {
		keywords = []string{}
	}
	return update.SetRead(flags.Seen).SetAnswered(flags.Answered).SetFlagged(flags.Flagged).SetImapDeleted(flags.Deleted).SetDraft(flags.Draft).SetKeywords(keywords)
}
