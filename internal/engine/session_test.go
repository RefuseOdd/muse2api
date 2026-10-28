package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRenewSessionHTTPMatrix(t *testing.T) {
	oldSession, oldWake := museSessionURL, museWakeURL
	defer func() { museSessionURL, museWakeURL = oldSession, oldWake }()

	type scenario struct {
		name        string
		sessionCode int
		sessionBody string
		redirect    bool
		verifyState string
		wake        bool
		wantAuth    bool
		wantOK      bool
		wantWakeOK  bool
		wantState   string
	}
	scenarios := []scenario{
		{name: "401", sessionCode: 401, wantAuth: true},
		{name: "403", sessionCode: 403},
		{name: "429", sessionCode: 429},
		{name: "302", sessionCode: 302, redirect: true},
		{name: "non-json", sessionCode: 200, sessionBody: "not json"},
		{name: "unassigned", sessionCode: 200, sessionBody: `{"status":"pending"}`},
		{name: "assigned", sessionCode: 200, sessionBody: `{"status":"assigned","vm_id":"vm-1","vm_state":"SUSPENDED"}`, wantOK: true, wantState: "SUSPENDED"},
		{name: "wake-running", sessionCode: 200, sessionBody: `{"status":"assigned","vm_id":"vm-1","vm_state":"SUSPENDED"}`, verifyState: "RUNNING", wake: true, wantOK: true, wantWakeOK: true, wantState: "RUNNING"},
		{name: "wake-suspended", sessionCode: 200, sessionBody: `{"status":"assigned","vm_id":"vm-1","vm_state":"SUSPENDED"}`, verifyState: "SUSPENDED", wake: true, wantOK: true, wantWakeOK: false, wantState: "SUSPENDED"},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			var sessionCalls int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/session":
					call := atomic.AddInt32(&sessionCalls, 1)
					if tc.redirect {
						w.Header().Set("Location", "/should-not-be-followed")
						w.WriteHeader(http.StatusFound)
						return
					}
					if tc.sessionCode != 200 {
						w.WriteHeader(tc.sessionCode)
						return
					}
					if call > 1 && tc.verifyState != "" {
						_, _ = w.Write([]byte(`{"status":"assigned","vm_id":"vm-1","vm_state":"` + tc.verifyState + `"}`))
						return
					}
					if call == 1 {
						http.SetCookie(w, &http.Cookie{Name: "hatch_sess", Value: "renewed", MaxAge: 3600})
					}
					_, _ = w.Write([]byte(tc.sessionBody))
				case "/api/hatch/vm/wake":
					if r.Method != http.MethodPost {
						t.Errorf("wake must use POST")
					}
					_, _ = w.Write([]byte(`{"accepted":true}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			museSessionURL = server.URL + "/api/session"
			museWakeURL = server.URL + "/api/hatch/vm/wake"

			eng := newTestEngine(false, false)
			res, err := eng.RenewSessionHTTP(context.Background(), map[string]string{"hatch_sess": "old", "hatch_vml": "v"}, map[string]int64{"hatch_vml": 123}, tc.wake)
			if tc.wantAuth {
				if !IsAuthError(err) {
					t.Fatalf("expected auth error, got %v", err)
				}
				return
			}
			if tc.sessionCode != 200 {
				if err == nil || IsAuthError(err) {
					t.Fatalf("expected a generation error, got %v", err)
				}
				if !strings.Contains(err.Error(), "HTTP") {
					t.Fatalf("error should mention the HTTP status: %v", err)
				}
				return
			}
			if !tc.wantOK {
				if err == nil || IsAuthError(err) {
					t.Fatalf("expected a generation error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !res.OK || res.Status != "assigned" {
				t.Fatalf("unexpected renewal result: %+v", res)
			}
			if res.Cookies["hatch_sess"] != "renewed" {
				t.Fatalf("response cookie was not merged: %+v", res.Cookies)
			}
			if res.CookiesExp["hatch_sess"] <= time.Now().Unix() {
				t.Fatalf("cookie expiry was not recorded: %+v", res.CookiesExp)
			}
			if tc.wake {
				if !res.WakeRequested {
					t.Fatalf("expected a wake request")
				}
				if res.WakeOK != tc.wantWakeOK {
					t.Fatalf("wake_ok=%v, want %v (state=%s)", res.WakeOK, tc.wantWakeOK, res.VMState)
				}
				if res.VMState != tc.wantState {
					t.Fatalf("vm_state=%q, want %q", res.VMState, tc.wantState)
				}
			}
		})
	}
}

func TestRenewSessionHTTPWakeDisabled(t *testing.T) {
	oldSession := museSessionURL
	defer func() { museSessionURL = oldSession }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"assigned","vm_id":"vm-1","vm_state":"DISABLED"}`))
	}))
	defer server.Close()
	museSessionURL = server.URL
	eng := newTestEngine(false, false)
	res, err := eng.RenewSessionHTTP(context.Background(), map[string]string{"hatch_sess": "x"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.WakeRequested {
		t.Fatalf("a DISABLED VM must not be woken")
	}
}
