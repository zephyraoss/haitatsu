package routing

import (
	"context"
	"path"
	"strings"

	"github.com/zephyraoss/haitatsu/internal/database/ent"
	"github.com/zephyraoss/haitatsu/internal/database/ent/mailbox"
	"github.com/zephyraoss/haitatsu/internal/database/ent/route"
)

type Resolver struct {
	client *ent.Client
}

type Result struct {
	OriginalRecipient string
	BaseRecipient     string
	PlusTag           string
	RouteID           string
	Mailboxes         []*ent.Mailbox
}

type candidate struct {
	LookupAddress     string
	OriginalRecipient string
	BaseRecipient     string
	PlusTag           string
}

func NewResolver(client *ent.Client) *Resolver {
	return &Resolver{client: client}
}

func (r *Resolver) Resolve(ctx context.Context, recipient string) (Result, bool, error) {
	address := normalizeAddress(recipient)
	base, tag := splitPlusTag(address)
	literal := candidate{LookupAddress: address, OriginalRecipient: address, BaseRecipient: address}

	if tag == "" {
		if result, ok, err := r.exactMailbox(ctx, literal); err != nil || ok {
			return result, ok, err
		}
	}
	if result, ok, err := r.exactRoute(ctx, literal); err != nil || ok {
		return result, ok, err
	}

	target := candidate{LookupAddress: base, OriginalRecipient: address, BaseRecipient: base, PlusTag: tag}
	if tag != "" {
		if result, ok, err := r.exactMailbox(ctx, target); err != nil || ok {
			return result, ok, err
		}
		if result, ok, err := r.exactRoute(ctx, target); err != nil || ok {
			return result, ok, err
		}
	}
	if result, ok, err := r.patternRoute(ctx, target); err != nil || ok {
		return result, ok, err
	}
	return r.catchAllRoute(ctx, target)
}

func OverQuota(mbox *ent.Mailbox) bool {
	return mbox.QuotaBytes > 0 && mbox.UsedBytes >= mbox.QuotaBytes
}

func (r *Resolver) exactMailbox(ctx context.Context, target candidate) (Result, bool, error) {
	mbox, err := r.client.Mailbox.Query().Where(mailbox.PrimaryAddressEQ(target.LookupAddress), mailbox.StatusEQ("active"), mailbox.DeletedAtIsNil()).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return Result{}, false, nil
		}
		return Result{}, false, err
	}
	return Result{OriginalRecipient: target.OriginalRecipient, BaseRecipient: target.BaseRecipient, PlusTag: target.PlusTag, Mailboxes: []*ent.Mailbox{mbox}}, true, nil
}

func (r *Resolver) exactRoute(ctx context.Context, target candidate) (Result, bool, error) {
	route, err := r.client.Route.Query().Where(route.SourceAddressEQ(target.LookupAddress), route.StatusEQ("active"), route.DeletedAtIsNil()).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return Result{}, false, nil
		}
		return Result{}, false, err
	}
	return r.routeResult(ctx, route, target)
}

func (r *Resolver) patternRoute(ctx context.Context, target candidate) (Result, bool, error) {
	routes, err := r.client.Route.Query().Where(route.TypeEQ("pattern"), route.StatusEQ("active"), route.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return Result{}, false, err
	}
	for _, item := range routes {
		matched, err := path.Match(item.SourceAddress, target.OriginalRecipient)
		if err != nil {
			continue
		}
		if matched {
			return r.routeResult(ctx, item, target)
		}
	}
	return Result{}, false, nil
}

func (r *Resolver) catchAllRoute(ctx context.Context, target candidate) (Result, bool, error) {
	domain := addressDomain(target.OriginalRecipient)
	routes, err := r.client.Route.Query().Where(route.TypeEQ("catch_all"), route.StatusEQ("active"), route.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return Result{}, false, err
	}
	for _, item := range routes {
		if item.SourceAddress == "*@"+domain {
			return r.routeResult(ctx, item, target)
		}
	}
	return Result{}, false, nil
}

func (r *Resolver) routeResult(ctx context.Context, route *ent.Route, target candidate) (Result, bool, error) {
	mailboxes, err := r.client.Mailbox.Query().Where(mailbox.IDIn(route.Destinations...), mailbox.StatusEQ("active"), mailbox.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return Result{}, false, err
	}
	if len(mailboxes) == 0 {
		return Result{}, false, nil
	}
	return Result{OriginalRecipient: target.OriginalRecipient, BaseRecipient: target.BaseRecipient, PlusTag: target.PlusTag, RouteID: route.ID, Mailboxes: mailboxes}, true, nil
}

func normalizeAddress(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

func splitPlusTag(address string) (string, string) {
	local, domain, ok := strings.Cut(address, "@")
	if !ok {
		return address, ""
	}
	base, tag, ok := strings.Cut(local, "+")
	if !ok {
		return address, ""
	}
	return base + "@" + domain, tag
}

func addressDomain(address string) string {
	_, domain, _ := strings.Cut(address, "@")
	return domain
}
