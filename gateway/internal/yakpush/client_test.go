package yakpush

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCallbackAuthenticationValidationPersistenceAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yakphone.json")
	c, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		path, method, body string
		want               int
	}{
		{"/token/wrong", "POST", `{"voip_token":"` + token + `"}`, 404},
		{c.CallbackPath(), "GET", "", 405},
		{c.CallbackPath(), "POST", `{"voip_token":"invalid"}`, 400},
		{c.CallbackPath(), "POST", `{"voip_token":"` + token + `"} {}`, 400},
		{c.CallbackPath(), "POST", strings.Repeat("x", 5000), 400},
		{c.CallbackPath(), "POST", `{"voip_token":"` + token + `"}`, 200},
	} {
		w := httptest.NewRecorder()
		c.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.want {
			t.Fatalf("%s: got %d want %d", tc.method, w.Code, tc.want)
		}
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("credentials not private")
	}
	restored, err := Open(path, "old-config-token")
	if err != nil {
		t.Fatal(err)
	}
	if restored.CallbackPath() != c.CallbackPath() || restored.credentials.Token != token {
		t.Fatal("token or callback lost at restart")
	}
	token2 := strings.Repeat("cd", 32)
	w := httptest.NewRecorder()
	restored.ServeHTTP(w, httptest.NewRequest("POST", c.CallbackPath(), strings.NewReader(`{"voip_token":"`+token2+`"}`)))
	if w.Code != 200 || restored.credentials.Token != token2 {
		t.Fatal("rotation failed")
	}
}

func TestRelayUsesUpdatedTokenAndReportsRejectionsWithoutSecrets(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "yakphone.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background(), "sip:fixture@host", "voip", ""); err != ErrNoToken {
		t.Fatalf("missing token: %v", err)
	}
	token := strings.Repeat("ab", 32)
	c.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", c.CallbackPath(), strings.NewReader(`{"voip_token":"`+token+`"}`)))
	for _, status := range []int{200, 400, 401, 403, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["token"] != token || body["type"] != "voip" || body["caller_uri"] != "sip:fixture@host" {
				t.Error("bad relay payload")
			}
			w.WriteHeader(status)
			if status == 200 {
				fmt.Fprint(w, `{"success":true}`)
			} else {
				fmt.Fprint(w, token)
			}
		}))
		c.endpoint = server.URL
		err := c.Send(context.Background(), "sip:fixture@host", "voip", "")
		server.Close()
		if (err == nil) != (status == 200) {
			t.Fatalf("HTTP %d result %v", status, err)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Fatal("error leaked token")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"success":false,"error":"secret"}`) }))
	defer server.Close()
	c.endpoint = server.URL
	if err := c.Send(context.Background(), "sip:fixture@host", "voip", ""); err == nil {
		t.Fatal("application error accepted")
	}
}

func TestConcurrentTokenRotationAndDispatch(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "yakphone.json"), strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"success":true}`) }))
	defer server.Close()
	c.endpoint = server.URL
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c.Send(context.Background(), "sip:fixture@host", "voip", "") }()
		go func() {
			defer wg.Done()
			c.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", c.CallbackPath(), strings.NewReader(`{"voip_token":"`+strings.Repeat("cd", 32)+`"}`)))
		}()
	}
	wg.Wait()
}
