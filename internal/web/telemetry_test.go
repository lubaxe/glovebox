package web

import (
	"net/http"
	"testing"
)

func TestTelemetryAccessChecks(t *testing.T) {
	ts := clipServer(t)
	for _, c := range []struct {
		path   string
		status int
	}{
		{"/api/telemetry/snap-000001/TeslaCam/secret.txt", http.StatusBadRequest},
		{"/api/telemetry/snap-000002/" + sentryDir + "/2026-10-15_00-39-00-front.mp4", http.StatusNotFound},
		{"/api/telemetry/snap-000001/TeslaCam/../private.mp4", http.StatusNotFound},
		{"/api/telemetry/snap-000001/TeslaCam/EncryptedClips/recording.mp4", http.StatusBadRequest},
		{"/api/telemetry/snap-000001/" + sentryDir + "/2026-10-15_00-39-00-front.mp4", http.StatusOK},
	} {
		response, err := http.Get(ts.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.path, response.StatusCode, c.status)
		}
	}
}
