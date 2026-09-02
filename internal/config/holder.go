package config

import "sync/atomic"

type Holder struct {
	current atomic.Pointer[Config]
}

func NewHolder(cfg *Config) *Holder {
	h := &Holder{}
	h.current.Store(cfg)
	return h
}

func (h *Holder) Get() *Config {
	return h.current.Load()
}

func (h *Holder) Set(cfg *Config) {
	h.current.Store(cfg)
}
