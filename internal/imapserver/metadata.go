package imapserver

import (
	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/mailparse"
)

func messageMetadata(source *ent.Message) mailparse.Metadata {
	return mailparse.Metadata{
		RFCMessageID: source.RfcMessageID,
		Headers:      source.Headers,
		From:         source.FromAddresses,
		To:           source.ToAddresses,
		CC:           source.CcAddresses,
		BCC:          source.BccAddresses,
		Subject:      source.Subject,
		Date:         source.Date,
		TextExtract:  source.TextBodyExtract,
		HTMLExtract:  source.HTMLBodyExtract,
		Attachments:  source.Attachments,
	}
}
