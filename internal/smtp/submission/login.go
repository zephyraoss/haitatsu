package submission

import (
	"errors"

	"github.com/emersion/go-sasl"
)

type loginServer struct {
	authenticate func(username, password string) error
	username     string
	step         int
}

func newLoginServer(authenticate func(username, password string) error) sasl.Server {
	return &loginServer{authenticate: authenticate}
}

func (s *loginServer) Next(response []byte) ([]byte, bool, error) {
	switch s.step {
	case 0:
		s.step = 1
		if response == nil {
			return []byte("Username:"), false, nil
		}
		s.username = string(response)
		s.step = 2
		return []byte("Password:"), false, nil
	case 1:
		s.username = string(response)
		s.step = 2
		return []byte("Password:"), false, nil
	case 2:
		s.step = 3
		if s.username == "" {
			return nil, true, errors.New("username required")
		}
		return nil, true, s.authenticate(s.username, string(response))
	}
	return nil, true, errors.New("unexpected LOGIN response")
}
