// Package security authenticates internal HTTP calls with per-peer keys. Each
// signature binds the recipient, request, body and delegated actor metadata.
package security

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Auth struct {
	TLS                *tls.Config
	Roots              *x509.CertPool
	transport          *http.Transport
	Name               string
	Incoming, Outgoing map[string][]byte
	mu                 sync.Mutex
	seen               map[string]int64
	claimNonce         func(string, int64) (bool, error)
}
type callerKey struct{}

func Caller(r *http.Request) string {
	value, _ := r.Context().Value(callerKey{}).(string)
	return value
}
func Load(name string) (*Auth, error) {
	if os.Getenv("SERVICE_NAME") != name {
		return nil, errors.New("invalid SERVICE_NAME")
	}
	decode := func(variable string) (map[string][]byte, error) {
		var values map[string]string
		if err := json.Unmarshal([]byte(os.Getenv(variable)), &values); err != nil || values == nil {
			return nil, fmt.Errorf("invalid %s", variable)
		}
		result := map[string][]byte{}
		for peer, value := range values {
			key, err := base64.RawURLEncoding.DecodeString(value)
			if err != nil || len(key) != 32 {
				return nil, fmt.Errorf("invalid key in %s", variable)
			}
			result[peer] = key
		}
		return result, nil
	}
	incoming, err := decode("SERVICE_INCOMING_KEYS")
	if err != nil || len(incoming) == 0 {
		return nil, errors.New("SERVICE_INCOMING_KEYS required")
	}
	outgoing, err := decode("SERVICE_OUTGOING_KEYS")
	if err != nil {
		return nil, err
	}
	required := map[string][]string{"order": {"inventory", "payment", "audit-worker"}, "inventory": {"audit-worker"}, "payment": {"order", "audit-worker"}}
	for _, peer := range required[name] {
		if len(outgoing[peer]) != 32 {
			return nil, errors.New("missing required outbound service key")
		}
	}
	certificate, err := tls.LoadX509KeyPair(os.Getenv("SERVICE_TLS_CERT"), os.Getenv("SERVICE_TLS_KEY"))
	if err != nil {
		return nil, errors.New("valid SERVICE_TLS_CERT and SERVICE_TLS_KEY required")
	}
	ca, err := os.ReadFile(os.Getenv("SERVICE_CA_FILE"))
	if err != nil {
		return nil, errors.New("SERVICE_CA_FILE required")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid service CA")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	return &Auth{transport: transport, Name: name, Incoming: incoming, Outgoing: outgoing, TLS: &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}, Roots: roots}, nil
}
func body(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	value, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024+1))
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(value) > 1024*1024 {
		return nil, errors.New("payload_too_large")
	}
	r.Body = io.NopCloser(bytes.NewReader(value))
	return value, nil
}
func canonical(r *http.Request, recipient string, payload []byte) string {
	digest := sha256.Sum256(payload)
	return strings.Join([]string{r.Method, r.URL.RequestURI(), hex.EncodeToString(digest[:]), r.Header.Get("X-Service-ID"), recipient, r.Header.Get("X-Service-Time"), r.Header.Get("X-Service-Nonce"), r.Header.Get("X-Actor-ID"), r.Header.Get("X-Correlation-ID"), r.Header.Get("X-User-Scopes"), r.Header.Get("Idempotency-Key")}, "\n")
}
func signature(r *http.Request, recipient string, payload, key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(canonical(r, recipient, payload)))
	return mac.Sum(nil)
}
func (a *Auth) Sign(r *http.Request, target string) error {
	if a == nil || len(a.Outgoing[target]) != 32 {
		return errors.New("missing outbound service credential")
	}
	payload, err := body(r)
	if err != nil {
		return err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	r.Header.Set("X-Service-ID", a.Name)
	r.Header.Set("X-Service-Time", strconv.FormatInt(time.Now().Unix(), 10))
	r.Header.Set("X-Service-Nonce", hex.EncodeToString(nonce[:]))
	r.Header.Set("X-Service-Signature", hex.EncodeToString(signature(r, target, payload, a.Outgoing[target])))
	return nil
}
func (a *Auth) Do(client *http.Client, target string, r *http.Request) (*http.Response, error) {
	if err := a.Sign(r, target); err != nil {
		return nil, err
	}
	// Credentials and actor metadata must never follow a redirect to another host.
	safe := *client
	if a.transport != nil {
		safe.Transport = a.transport
	}
	if a.TLS != nil && r.URL.Scheme != "https" {
		return nil, errors.New("service HTTPS required")
	}
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return safe.Do(r)
}
func reject(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		caller := r.Header.Get("X-Service-ID")
		key := a.Incoming[caller]
		stamp, err := strconv.ParseInt(r.Header.Get("X-Service-Time"), 10, 64)
		now := time.Now().Unix()
		nonce := r.Header.Get("X-Service-Nonce")
		nonceBytes, nonceErr := hex.DecodeString(nonce)
		supplied, sigErr := hex.DecodeString(r.Header.Get("X-Service-Signature"))
		if err != nil || stamp < now-30 || stamp > now+30 || len(key) != 32 || nonceErr != nil || len(nonceBytes) != 16 || sigErr != nil || len(supplied) != 32 {
			reject(w, 401, "service_unauthorized")
			return
		}
		payload, err := body(r)
		if err != nil {
			reject(w, 413, "payload_too_large")
			return
		}
		if !hmac.Equal(supplied, signature(r, a.Name, payload, key)) {
			reject(w, 401, "service_unauthorized")
			return
		}
		if len(r.Header.Get("X-Actor-ID")) > 128 || len(r.Header.Get("X-Correlation-ID")) > 128 || len(r.Header.Get("X-User-Scopes")) > 128 || !Allowed(a.Name, caller, r) {
			reject(w, 403, "service_forbidden")
			return
		}
		if a.claimNonce != nil {
			accepted, err := a.claimNonce(caller+":"+nonce, stamp+30)
			if err != nil {
				reject(w, 503, "service_busy")
				return
			}
			if !accepted {
				reject(w, 401, "service_replay")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller)))
			return
		}
		a.mu.Lock()
		if a.seen == nil {
			a.seen = map[string]int64{}
		}
		for id, expiry := range a.seen {
			if expiry < now {
				delete(a.seen, id)
			}
		}
		id := caller + ":" + nonce
		_, replayed := a.seen[id]
		full := len(a.seen) >= 10000
		if !replayed && !full {
			a.seen[id] = stamp + 30
		}
		a.mu.Unlock()
		if replayed {
			reject(w, 401, "service_replay")
			return
		}
		if full {
			reject(w, 503, "service_busy")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller)))
	})
}
func Allowed(service, caller string, r *http.Request) bool {
	bff := caller == "desktop-bff" || caller == "app-bff"
	if bff && !strings.HasPrefix(r.Header.Get("X-Actor-ID"), "usr_") {
		return false
	}
	if r.URL.Path == "/v1/audit/events" {
		if r.Method == "GET" {
			return bff && strings.Contains(" "+r.Header.Get("X-User-Scopes")+" ", " audit:read ")
		}
		return service == "audit-worker" && r.Method == "POST" && (caller == "order" || caller == "inventory" || caller == "payment")
	}
	switch service {
	case "order":
		return (caller == "payment" && r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/orders/") && strings.HasPrefix(r.Header.Get("X-Actor-ID"), "usr_")) || bff && ((r.Method == "POST" && r.URL.Path == "/v1/checkouts") || (r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/checkouts/")) || ((r.Method == "GET" || r.Method == "POST") && strings.HasPrefix(r.URL.Path, "/v1/orders/")))
	case "inventory":
		return caller == "order" && ((r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/stock/")) || ((r.Method == "GET" || r.Method == "POST") && (r.URL.Path == "/v1/reservations" || strings.HasPrefix(r.URL.Path, "/v1/reservations/"))))
	case "payment":
		return (caller == "order" && ((r.Method == "POST" && r.URL.Path == "/v1/payments") || (r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/payments/")))) || (bff && ((r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/payments/")) || (r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/confirm"))))
	}
	return false
}

func (a *Auth) Listen(addr string, handler http.Handler) error {
	if a == nil || a.TLS == nil {
		return errors.New("service TLS required")
	}
	server := &http.Server{Addr: addr, Handler: a.Middleware(handler), TLSConfig: a.TLS, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	return server.ListenAndServeTLS("", "")
}

// Persist nonces in the service database so a restart cannot reopen the replay window.
func (a *Auth) UseReplayStore(db *sql.DB) error {
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS security_nonces(id TEXT PRIMARY KEY,expires INTEGER NOT NULL);CREATE INDEX IF NOT EXISTS security_nonce_expiry ON security_nonces(expires)"); err != nil {
		return err
	}
	a.claimNonce = func(id string, expires int64) (bool, error) {
		tx, err := db.Begin()
		if err != nil {
			return false, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("DELETE FROM security_nonces WHERE expires<?", time.Now().Unix()); err != nil {
			return false, err
		}
		var count int
		if err = tx.QueryRow("SELECT COUNT(*) FROM security_nonces").Scan(&count); err != nil {
			return false, err
		}
		if count >= 10000 {
			return false, errors.New("nonce store full")
		}
		result, err := tx.Exec("INSERT INTO security_nonces(id,expires) VALUES(?,?) ON CONFLICT(id) DO NOTHING", id, expires)
		if err != nil {
			return false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		return changed == 1, tx.Commit()
	}
	return nil
}
