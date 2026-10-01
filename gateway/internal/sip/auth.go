package sip

import (
	"crypto/md5" // SIP Digest MD5 interoperability; transport is the private tailnet.
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Auth struct {
	mu     sync.Mutex
	users  map[string]string
	realm  string
	nonces map[string]*digestNonce
}

type digestUse struct {
	request [32]byte
	at      time.Time
}

type digestCounter struct {
	high uint64
	uses map[uint64]digestUse
}

type digestNonce struct {
	created time.Time
	clients map[string]*digestCounter
}

const digestNonceTTL = 5 * time.Minute
const digestReplayWindow = 64
const maxDigestNonces = 64

func NewAuth(realm string) *Auth {
	return &Auth{users: make(map[string]string), realm: realm, nonces: make(map[string]*digestNonce)}
}

func (a *Auth) AddUser(username, password string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.users[username] = password
}
func (a *Auth) Check(username, password string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.users[username]
	return ok && subtle.ConstantTimeCompare([]byte(p), []byte(password)) == 1
}
func (a *Auth) Realm() string { return a.realm }
func (a *Auth) Nonce() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Every challenge has a fresh nonce. Keep earlier challenges valid so
	// REGISTER and INVITE authentication cannot invalidate each other.
	oldest := ""
	for nonce, state := range a.nonces {
		if time.Since(state.created) > digestNonceTTL {
			delete(a.nonces, nonce)
			continue
		}
		if oldest == "" || state.created.Before(a.nonces[oldest].created) {
			oldest = nonce
		}
	}
	if len(a.nonces) >= maxDigestNonces {
		delete(a.nonces, oldest)
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("SIP authentication requires secure randomness")
	}
	nonce := hex.EncodeToString(b)
	a.nonces[nonce] = &digestNonce{created: time.Now(), clients: make(map[string]*digestCounter)}
	return nonce
}

// Verify checks the advertised qop=auth Digest against the actual method,
// request URI and configured account. A header's presence proves nothing.
func (a *Auth) Verify(msg, username string) bool {
	ok, _, _ := a.verify(msg, username)
	return ok
}

// stale is returned only after checking the password proof; expired server
// state (including a restart) must prompt an automatic nonce refresh, not a
// misleading "wrong password" error on the phone.
func (a *Auth) verify(msg, username string) (ok, stale bool, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	request := strings.Fields(strings.SplitN(msg, "\r\n", 2)[0])
	if len(request) != 3 || request[2] != "SIP/2.0" {
		return false, false, "bad_request"
	}
	header := ""
	for _, line := range strings.Split(strings.SplitN(msg, "\r\n\r\n", 2)[0], "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Authorization") {
			if header != "" {
				return false, false, "duplicate_authorization"
			}
			header = strings.TrimSpace(value)
		}
	}
	fields, ok := parseDigest(header)
	if !ok || fields["username"] != username || fields["realm"] != a.realm || fields["nonce"] == "" || fields["uri"] != request[1] {
		return false, false, "missing_or_invalid_digest"
	}
	password, exists := a.users[username]
	if !exists || password == "" || fields["qop"] != "auth" || fields["cnonce"] == "" {
		return false, false, "account_or_qop"
	}
	if algorithm := fields["algorithm"]; algorithm != "" && !strings.EqualFold(algorithm, "MD5") {
		return false, false, "algorithm"
	}
	nc := fields["nc"]
	count, err := strconv.ParseUint(nc, 16, 32)
	if err != nil || len(nc) != 8 || count == 0 {
		return false, false, "nonce_count"
	}
	hash := func(value string) string {
		digest := md5.Sum([]byte(value))
		return hex.EncodeToString(digest[:])
	}
	ha1 := hash(username + ":" + a.realm + ":" + password)
	ha2 := hash(request[0] + ":" + request[1])
	expected := hash(ha1 + ":" + fields["nonce"] + ":" + nc + ":" + fields["cnonce"] + ":auth:" + ha2)
	provided, err := hex.DecodeString(fields["response"])
	want, _ := hex.DecodeString(expected)
	if err != nil || subtle.ConstantTimeCompare(provided, want) != 1 {
		return false, false, "password_proof"
	}
	state, exists := a.nonces[fields["nonce"]]
	if !exists || time.Since(state.created) > digestNonceTTL {
		return false, true, "stale_nonce"
	}
	key := username + "\x00" + fields["cnonce"]
	requestHash := sha256.Sum256([]byte(msg))
	counter := state.clients[key]
	if counter == nil {
		if len(state.clients) >= 32 {
			return false, true, "nonce_capacity"
		}
		counter = &digestCounter{uses: make(map[uint64]digestUse)}
		state.clients[key] = counter
	}
	if previous, exists := counter.uses[count]; exists {
		if previous.request == requestHash && time.Since(previous.at) < 32*time.Second {
			return true, false, "retransmission"
		}
		// The proof is correct, but this nonce/count is no longer usable.
		// Issue a new challenge without asking the user to change passwords.
		return false, true, "nonce_count_reused"
	}
	if count+digestReplayWindow <= counter.high {
		return false, true, "nonce_count_too_old"
	}
	if count > counter.high {
		counter.high = count
		for n := range counter.uses {
			if n+digestReplayWindow <= count {
				delete(counter.uses, n)
			}
		}
	}
	// Remember several concurrent transactions, not just the newest count:
	// a REGISTER can overtake an INVITE retransmission on UDP.
	counter.uses[count] = digestUse{request: requestHash, at: time.Now()}
	return true, false, "verified"
}

var digestField = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z0-9_-]*)\s*=\s*("(?:[^"\\]|\\.)*"|[^,\s]+)\s*(,|$)`)

func parseDigest(header string) (map[string]string, bool) {
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Digest") {
		return nil, false
	}
	fields := make(map[string]string)
	for strings.TrimSpace(rest) != "" {
		match := digestField.FindStringSubmatch(rest)
		if match == nil {
			return nil, false
		}
		key, value := strings.ToLower(match[1]), match[2]
		if _, duplicate := fields[key]; duplicate {
			return nil, false
		}
		if strings.HasPrefix(value, `"`) {
			var err error
			value, err = strconv.Unquote(value)
			if err != nil {
				return nil, false
			}
		}
		fields[key] = value
		rest = rest[len(match[0]):]
		if match[3] == "," && strings.TrimSpace(rest) == "" {
			return nil, false
		}
	}
	return fields, len(fields) > 0
}

func WWWAuthHeader(realm, nonce string) string {
	return `Digest realm="` + realm + `", nonce="` + nonce + `", algorithm=MD5, qop="auth"`
}
