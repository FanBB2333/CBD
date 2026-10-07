package sip

import (
	"net"
	"strings"
	"sync"
	"time"
)

type Registration struct {
	Username  string
	Contact   string
	Expires   time.Time
	Transport string
	LastSeen  time.Time
	// Received is the authenticated REGISTER's actual UDP flow. Contact can
	// name an unroutable phone VPN/proxy interface (for example 198.18.0.1).
	Received string
}

type Registrar struct {
	mu   sync.Mutex
	regs map[string]*Registration
}

func NewRegistrar() *Registrar { return &Registrar{regs: make(map[string]*Registration)} }

func (r *Registrar) Register(username, contact, transport string, expiresSec int) {
	r.RegisterFrom(username, contact, transport, expiresSec, nil)
}

func (r *Registrar) RegisterFrom(username, contact, transport string, expiresSec int, remote *net.UDPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	received := ""
	if remote != nil {
		received = remote.String()
	}
	if expiresSec == 0 {
		// A retiring SIP transport must not unregister the replacement flow.
		if previous := r.regs[username]; previous != nil && strings.TrimSpace(contact) != "*" {
			if received != "" && previous.Received != "" && received != previous.Received {
				return
			}
			if contactURI(contact, nil, username) != contactURI(previous.Contact, nil, username) {
				return
			}
		}
		delete(r.regs, username)
		return
	}
	r.regs[username] = &Registration{
		Username:  username,
		Contact:   contact,
		Transport: transport,
		Received:  received,
		LastSeen:  time.Now(),
		Expires:   time.Now().Add(time.Duration(expiresSec) * time.Second),
	}
}

func (r *Registration) RemoteAddr() *net.UDPAddr {
	if r.Received != "" {
		addr, err := net.ResolveUDPAddr("udp", r.Received)
		if err == nil {
			return addr
		}
	}
	return contactAddr(r.Contact)
}

func (r *Registrar) Get(username string) (*Registration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.regs[username]
	if !ok {
		return nil, false
	}
	if time.Now().After(reg.Expires) {
		delete(r.regs, username)
		return nil, false
	}
	return reg, true
}

func (r *Registrar) All() []*Registration {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Registration
	for _, v := range r.regs {
		if time.Now().Before(v.Expires) {
			out = append(out, v)
		}
	}
	return out
}
