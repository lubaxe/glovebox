package web

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/lubaxe/glovebox/internal/wifi"
)

type fakeWiFi struct {
	done   bool
	joined string
}

func (f *fakeWiFi) Status(context.Context) wifi.Status {
	return wifi.Status{AccessPoint: "Glovebox-ABCD", SetupDone: f.done}
}
func (f *fakeWiFi) Scan(context.Context) ([]wifi.Network, error) {
	return []wifi.Network{{SSID: "Home", Signal: 70, Secure: true}}, nil
}
func (f *fakeWiFi) Join(_ context.Context, ssid, password, country string) wifi.JoinResult {
	f.joined = ssid + "/" + password + "/" + country
	return wifi.JoinResult{SSID: ssid, OK: true, Address: "192.168.1.50"}
}
func (f *fakeWiFi) FinishSetup() error { f.done = true; return nil }

func TestWiFiRoutes(t *testing.T) {
	f := &fakeWiFi{}
	s := &Server{Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), WiFi: f}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if code, out := call(t, "GET", ts.URL+"/api/wifi", ""); code != 200 || out["access_point"] != "Glovebox-ABCD" || out["setup_done"] != false {
		t.Errorf("status: %d %v", code, out)
	}
	if code, out := call(t, "POST", ts.URL+"/api/wifi/join", `{"ssid":"Home","password":"correct horse","country":"CA"}`); code != 200 || out["ok"] != true || f.joined != "Home/correct horse/CA" {
		t.Errorf("join: %d %v %q", code, out, f.joined)
	}
	if code, out := call(t, "POST", ts.URL+"/api/setup/finish", `{}`); code != 200 || out["setup_done"] != true {
		t.Errorf("finish: %d %v", code, out)
	}
}

func TestNoWiFi(t *testing.T) {
	ts := newServer(fakeStatus{})
	defer ts.Close()
	if code, _ := call(t, "GET", ts.URL+"/api/wifi", ""); code != 503 {
		t.Errorf("status = %d, want 503 when Wi-Fi isn't managed", code)
	}
}
