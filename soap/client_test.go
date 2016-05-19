package soap

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	type msgT struct{ A, B string }
	type envT struct{ Body struct{ Message msgT } }
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if v := r.Header.Get("X-Test"); v != "true" {
			http.NotFound(w, r)
			return
		}
		io.Copy(w, r.Body)
	})
	s := httptest.NewServer(echo)
	defer s.Close()
	pre := func(r *http.Request) { r.Header.Set("X-Test", "true") }
	cases := []struct {
		C       *Client
		In, Out Message
		Fail    bool
	}{
		{
			C:   &Client{URL: s.URL, Pre: pre},
			In:  &msgT{A: "hello", B: "world"},
			Out: &envT{},
		},
		{
			C:   &Client{URL: s.URL, Pre: pre},
			In:  &msgT{A: "foo", B: "bar"},
			Out: &envT{},
		},
		{
			C:    &Client{URL: "", Pre: pre},
			Out:  &envT{},
			Fail: true,
		},
	}
	for i, tc := range cases {
		err := tc.C.RoundTrip(tc.In, tc.Out)
		if err != nil && !tc.Fail {
			t.Errorf("test %d: %v", i, err)
			continue
		}
		if tc.Fail {
			continue
		}
		env, ok := tc.Out.(*envT)
		if !ok {
			t.Errorf("test %d: response to %#v is not an envelope", i, tc.In)
			continue
		}
		if !reflect.DeepEqual(env.Body.Message, *tc.In.(*msgT)) {
			t.Errorf("test %d: message mismatch\nwant: %#v\nhave: %#v",
				i, tc.In, &env.Body.Message)
			continue
		}
	}
}
