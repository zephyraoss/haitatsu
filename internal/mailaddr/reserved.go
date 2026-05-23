package mailaddr

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

const ReservedBouncesLocal = "bounces"

var ErrReservedLocalPart = errors.New("local part bounces is reserved for system VERP")

func ReservedLocalPart(local string) bool {
	return strings.EqualFold(strings.TrimSpace(local), ReservedBouncesLocal)
}

func LocalPart(address string) (string, error) {
	parsed, err := mail.ParseAddress(strings.TrimSpace(address))
	if err != nil {
		local, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
		if !ok || local == "" {
			return "", fmt.Errorf("invalid address")
		}
		return local, nil
	}
	local, _, ok := strings.Cut(strings.ToLower(parsed.Address), "@")
	if !ok {
		return "", fmt.Errorf("invalid address")
	}
	return local, nil
}

func ValidateAddressNotReserved(address string) error {
	local, err := LocalPart(address)
	if err != nil {
		return err
	}
	if ReservedLocalPart(local) {
		return ErrReservedLocalPart
	}
	return nil
}
