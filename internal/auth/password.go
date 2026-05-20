package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password is required")
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	encodedSalt := base64.RawStdEncoding.EncodeToString(salt)
	encodedKey := base64.RawStdEncoding.EncodeToString(key)

	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, encodedSalt, encodedKey), nil
}

func VerifyPassword(password, encoded string) (bool, error) {
	params, salt, key, err := parseHash(encoded)
	if err != nil {
		return false, err
	}

	actual := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(actual, key) == 1, nil
}

type hashParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func parseHash(encoded string) (hashParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" || parts[1] != "v=19" {
		return hashParams{}, nil, nil, errors.New("invalid password hash")
	}

	params, err := parseParams(parts[2])
	if err != nil {
		return hashParams{}, nil, nil, err
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return hashParams{}, nil, nil, err
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return hashParams{}, nil, nil, err
	}

	return params, salt, key, nil
}

func parseParams(value string) (hashParams, error) {
	var params hashParams
	for _, part := range strings.Split(value, ",") {
		name, raw, ok := strings.Cut(part, "=")
		if !ok {
			return hashParams{}, errors.New("invalid password hash params")
		}
		number, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return hashParams{}, err
		}
		switch name {
		case "m":
			params.memory = uint32(number)
		case "t":
			params.time = uint32(number)
		case "p":
			params.threads = uint8(number)
		default:
			return hashParams{}, errors.New("unknown password hash param")
		}
	}
	if params.memory == 0 || params.time == 0 || params.threads == 0 {
		return hashParams{}, errors.New("incomplete password hash params")
	}
	return params, nil
}
