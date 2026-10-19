package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lubaxe/glovebox/internal/archive"
	"github.com/lubaxe/glovebox/internal/crypt"
)

type fakeEncryption struct {
	st        crypt.Status
	requested bool
	unlocked  string // identity used
}

func (f *fakeEncryption) Status() crypt.Status { return f.st }

func (f *fakeEncryption) SetRecipient(_ context.Context, r string) error {
	if !strings.HasPrefix(r, "age1") {
		return errors.New("that isn't an age public key")
	}
	f.st.Recipient = r
	return nil
}

func (f *fakeEncryption) UnlockWithKey(_ context.Context, id string) error {
	if id != "AGE-SECRET-KEY-1RIGHT" {
		return crypt.ErrWrongKey
	}
	f.unlocked, f.st.State = id, crypt.Unlocked
	return nil
}

func (f *fakeEncryption) RequestEncrypt() error {
	if f.st.Recipient == "" {
		return errors.New("set your key first")
	}
	f.requested = true
	return nil
}

type fakeCar struct {
	Car
	paired bool
}

func (c fakeCar) Paired() (time.Time, bool) { return time.Time{}, c.paired }

type lastRunArchive struct{ st archive.Status }

func (a lastRunArchive) Status() archive.Status { return a.st }
func (a lastRunArchive) Trigger()               {}

func TestEncryptionAPI(t *testing.T) {
	enc := &fakeEncryption{st: crypt.Status{State: crypt.Off, CarCheck: crypt.Enforce}}
	restarted := false
	s := &Server{Version: "test", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Encryption: enc, Restart: func() { restarted = true }}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	var got encryptionResponse
	decode := func(resp *http.Response) {
		t.Helper()
		got = encryptionResponse{}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
	}
	decode(do(t, "GET", ts.URL+"/api/encryption"))
	if got.Encrypted || got.Ready.Key || got.Ready.Car || got.Ready.Archived {
		t.Errorf("fresh: %+v", got)
	}

	if resp := sendJSON(t, "PUT", ts.URL+"/api/encryption/recipient", `{"recipient":"nope"}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad key: status %d", resp.StatusCode)
	}
	decode(sendJSON(t, "PUT", ts.URL+"/api/encryption/recipient", `{"recipient":"age1abc"}`))
	if !got.Ready.Key || got.Recipient != "age1abc" {
		t.Errorf("after setting the key: %+v", got)
	}

	// not without the car paired and everything backed up
	if resp := sendJSON(t, "POST", ts.URL+"/api/encryption/encrypt", `{"confirm":"ERASE"}`); resp.StatusCode != http.StatusUnprocessableEntity || enc.requested {
		t.Errorf("unready: status %d, requested %v", resp.StatusCode, enc.requested)
	}
	s.Car = fakeCar{paired: true}
	s.Archive = lastRunArchive{st: archive.Status{Last: &archive.Result{Pending: 3}}}
	if resp := sendJSON(t, "POST", ts.URL+"/api/encryption/encrypt", `{"confirm":"ERASE"}`); resp.StatusCode != http.StatusUnprocessableEntity || enc.requested {
		t.Errorf("files left to back up: status %d, requested %v", resp.StatusCode, enc.requested)
	}
	s.Archive = lastRunArchive{st: archive.Status{Last: &archive.Result{Uploaded: 5}}}
	if resp := sendJSON(t, "POST", ts.URL+"/api/encryption/encrypt", `{"confirm":"erase"}`); resp.StatusCode != http.StatusUnprocessableEntity || enc.requested {
		t.Errorf("not confirmed: status %d", resp.StatusCode)
	}
	decode(sendJSON(t, "POST", ts.URL+"/api/encryption/encrypt", `{"confirm":"ERASE"}`))
	if !enc.requested || !restarted || !got.Ready.Car || !got.Ready.Archived {
		t.Errorf("requested %v restarted %v: %+v", enc.requested, restarted, got)
	}

	// a form post from another site is refused
	resp, err := http.Post(ts.URL+"/api/encryption/unlock", "application/x-www-form-urlencoded", strings.NewReader("identity=x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form post: status %d", resp.StatusCode)
	}
	if resp := sendJSON(t, "POST", ts.URL+"/api/encryption/unlock", `{"identity":"AGE-SECRET-KEY-1WRONG"}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("wrong key: status %d", resp.StatusCode)
	}
	decode(sendJSON(t, "POST", ts.URL+"/api/encryption/unlock", `{"identity":"AGE-SECRET-KEY-1RIGHT"}`))
	if got.State != crypt.Unlocked || enc.unlocked == "" {
		t.Errorf("after unlocking: %+v", got)
	}
}

func TestEncryptionOff(t *testing.T) {
	s := &Server{Version: "test", Status: fakeStatus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp := do(t, "GET", ts.URL+"/api/encryption"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d", resp.StatusCode)
	}
}
