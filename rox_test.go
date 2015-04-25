package rox

import (
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func init() {}

func TestReverseProxy(t *testing.T) {
	const backendResponse = "I am the backend"
	const backendStatus = 200
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(backendStatus)
		w.Write([]byte(backendResponse))
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyHandler := New(backendURL)
	frontend := httptest.NewServer(proxyHandler)
	defer frontend.Close()

	getReq, _ := http.NewRequest("GET", frontend.URL, nil)
	getReq.Close = true

	res, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	bodyBytes, _ := ioutil.ReadAll(res.Body)
	if g, e := string(bodyBytes), backendResponse; g != e {
		t.Errorf("got body %q; expected %q", g, e)
	}

	if s, c := res.StatusCode, backendStatus; s != c {
		t.Errorf("res.StatusCode %q; is different from bkend status code %q", s, c)
	}
}
