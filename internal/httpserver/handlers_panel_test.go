package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The page asks this on every poll; unenrolled it must simply say so, not error.
func TestPanelStatusUnenrolled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/panel")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/panel = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Enrolled bool `json:"enrolled"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Enrolled {
		t.Error("a machine with no panel config reports enrolled=true")
	}
}

// The page reads this person's own usage over time through the local server, which
// asks the panel with the machine's enrolment: the token never reaches the page,
// the grain is checked before anything leaves the machine, and a machine that is
// connected to nothing says so instead of failing.
func TestTelemetryIsReadFromThePanelWithTheMachinesEnrolment(t *testing.T) {
	var gotAuth, gotQuery string
	panelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		if r.URL.Path != "/api/v1/telemetry" {
			http.NotFound(w, r)
			return
		}
		if gotAuth != "Bearer device-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"res":"minute","bucketSeconds":60,"points":[{"t":"2026-10-27T12:00:00Z","weighted":1500,"tokens":9000,"requests":4}]}`))
	}))
	defer panelSrv.Close()

	srv, home := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Not connected to a panel yet.
	resp, err := http.Get(ts.URL + "/api/telemetry?res=minute")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unenrolled = %d, want 400 with the page's own words", resp.StatusCode)
	}

	// A grain the panel does not have is refused here, without asking it.
	if err := os.MkdirAll(filepath.Join(home, ".clawdh"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"server":"` + panelSrv.URL + `","deviceId":"d1","token":"device-token","personName":"Alice"}`
	if err := os.WriteFile(filepath.Join(home, ".clawdh", "panel-client.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	gotQuery = ""
	if resp, _ := http.Get(ts.URL + "/api/telemetry?res=fortnight"); resp.StatusCode != http.StatusBadRequest || gotQuery != "" {
		t.Errorf("a bad grain = %d and reached the panel with %q, want 400 and no call", resp.StatusCode, gotQuery)
	}

	resp, err = http.Get(ts.URL + "/api/telemetry?res=minute&since=2026-10-27T11:59:00Z")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Res    string `json:"res"`
		Points []struct {
			Weighted float64 `json:"weighted"`
			Requests int     `json:"requests"`
		} `json:"points"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || out.Res != "minute" || len(out.Points) != 1 || out.Points[0].Weighted != 1500 || out.Points[0].Requests != 4 {
		t.Fatalf("telemetry = %d %+v, want the panel's one minute", resp.StatusCode, out)
	}
	if gotAuth != "Bearer device-token" {
		t.Errorf("the panel saw %q, want this machine's enrolment", gotAuth)
	}
	if want := "res=minute&since=2026-10-27T11%3A59%3A00Z"; gotQuery != want {
		t.Errorf("the panel was asked %q, want %q — the cursor passed on so only the newest bucket comes back", gotQuery, want)
	}
}
