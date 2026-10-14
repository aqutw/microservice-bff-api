package security

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func fixture() (*Auth, *Auth) {
	key := bytes.Repeat([]byte{8}, 32)
	return &Auth{Name: "desktop-bff", Outgoing: map[string][]byte{"order": key}}, &Auth{Name: "order", Incoming: map[string][]byte{"desktop-bff": key}}
}
func signed(t *testing.T, sender *Auth) *http.Request {
	t.Helper()
	r := httptest.NewRequest("GET", "http://order/v1/orders/o", nil)
	r.Header.Set("X-Actor-ID", "usr_alice")
	if err := sender.Sign(r, "order"); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestRequestBindingAndReplay(t *testing.T) {
	for _, change := range []string{"valid", "actor", "path", "body", "time", "nonce", "missing"} {
		t.Run(change, func(t *testing.T) {
			sender, receiver := fixture()
			r := signed(t, sender)
			switch change {
			case "actor":
				r.Header.Set("X-Actor-ID", "usr_bob")
			case "path":
				r.URL.Path = "/v1/orders/other"
			case "body":
				r.Body = ioReader("changed")
			case "time":
				r.Header.Set("X-Service-Time", "1")
			case "nonce":
				r.Header.Set("X-Service-Nonce", "bad")
			case "missing":
				r.Header.Del("X-Service-Signature")
			}
			handler := receiver.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if Caller(r) != "desktop-bff" {
					t.Fatal("untrusted caller")
				}
				w.WriteHeader(204)
			}))
			output := httptest.NewRecorder()
			handler.ServeHTTP(output, r)
			if change == "valid" {
				if output.Code != 204 {
					t.Fatal(output.Code)
				}
				second := httptest.NewRecorder()
				handler.ServeHTTP(second, r)
				if second.Code != 401 {
					t.Fatal("replay accepted")
				}
			} else if output.Code != 401 {
				t.Fatalf("tamper accepted: %d", output.Code)
			}
		})
	}
}
func TestConcurrentReplayAllowsOneCall(t *testing.T) {
	sender, receiver := fixture()
	original := signed(t, sender)
	handler := receiver.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	var mu sync.Mutex
	accepted := 0
	var group sync.WaitGroup
	for i := 0; i < 24; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			r := original.Clone(original.Context())
			output := httptest.NewRecorder()
			handler.ServeHTTP(output, r)
			if output.Code == 204 {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	if accepted != 1 {
		t.Fatalf("accepted=%d", accepted)
	}
}
func TestPermissionsAreSpecificToServiceAndOperation(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/payments/p/confirm", nil)
	r.Header.Set("X-Actor-ID", "usr_alice")
	if Allowed("payment", "order", r) || !Allowed("payment", "desktop-bff", r) {
		t.Fatal("payment confirm permission")
	}
	r = httptest.NewRequest("GET", "/v1/audit/events", nil)
	r.Header.Set("X-Actor-ID", "usr_alice")
	if Allowed("audit-worker", "desktop-bff", r) {
		t.Fatal("missing audit scope")
	}
	r.Header.Set("X-User-Scopes", "audit:read")
	if !Allowed("audit-worker", "desktop-bff", r) || Allowed("audit-worker", "inventory", r) {
		t.Fatal("audit scope permission")
	}
}

func TestOnlyOrderCanCompensatePayments(t *testing.T) {
	for _, action := range []string{"cancel", "refund"} {
		for _, caller := range []string{"order", "desktop-bff", "app-bff", "inventory"} {
			r := httptest.NewRequest("POST", "/v1/payments/p/"+action, nil)
			r.Header.Set("X-Actor-ID", "usr_alice")
			if got := Allowed("payment", caller, r); got != (caller == "order") {
				t.Fatalf("%s %s allowed=%v", caller, action, got)
			}
		}
	}
}
func ioReader(value string) *readerCloser { return &readerCloser{strings.NewReader(value)} }

type readerCloser struct{ *strings.Reader }

func (*readerCloser) Close() error { return nil }

func TestInternalClientRequiresHTTPSAndTrustedCertificates(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	sender := &Auth{Name: "order", Outgoing: map[string][]byte{"inventory": bytes.Repeat([]byte{1}, 32)}, TLS: &tls.Config{}}
	sender.transport = http.DefaultTransport.(*http.Transport).Clone()
	sender.transport.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS13}
	request, _ := http.NewRequest("GET", server.URL, nil)
	if response, err := sender.Do(&http.Client{}, "inventory", request); err == nil {
		response.Body.Close()
		t.Fatal("untrusted certificate accepted")
	}
	sender.transport = server.Client().Transport.(*http.Transport)
	request, _ = http.NewRequest("GET", server.URL, nil)
	response, err := sender.Do(&http.Client{}, "inventory", request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal(response.StatusCode)
	}
	request, _ = http.NewRequest("GET", "http://inventory/v1/stock/mug", nil)
	if _, err = sender.Do(&http.Client{}, "inventory", request); err == nil {
		t.Fatal("plain HTTP accepted")
	}
}
