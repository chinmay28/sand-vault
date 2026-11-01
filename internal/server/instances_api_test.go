package server

import (
	"net/http"
	"testing"
)

// The register of machines and the pull, over HTTP; the vault package tests
// what a pull does.
func TestInstancesAndSyncOverHTTP(t *testing.T) {
	c := newTestClient(t)
	c.setup("a password", 3)
	v := c.server.vault
	v.AwaitBackupSync()

	w, body := c.json(http.MethodGet, "/api/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	if name, _ := body["instance"].(string); name != v.Instance() {
		t.Errorf("status names this machine %q, want %q", name, v.Instance())
	}
	if peers, _ := body["peers"].(float64); peers != 0 {
		t.Errorf("status counts %v peers on a vault that runs on one machine", body["peers"])
	}

	w, body = c.json(http.MethodGet, "/api/vault/instances", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("instances: %d %s", w.Code, w.Body.String())
	}
	instances, _ := body["instances"].([]any)
	if len(instances) != 1 {
		t.Fatalf("instances: %v, want this machine alone", body)
	}
	self, _ := instances[0].(map[string]any)
	if here, _ := self["here"].(bool); !here || self["name"] != v.Instance() {
		t.Errorf("the one machine listed is %v, want this one", self)
	}
	if accounts, _ := self["accounts"].([]any); len(accounts) != 3 {
		t.Errorf("this machine reaches %v, want its three folders", self["accounts"])
	}

	w, body = c.json(http.MethodPost, "/api/vault/sync", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", w.Code, w.Body.String())
	}
	if body["outcome"] != "current" {
		t.Errorf("a pull with nothing new came back %v", body["outcome"])
	}
	if asked, _ := body["asked"].(float64); asked != 3 {
		t.Errorf("the pull asked %v accounts, want 3", body["asked"])
	}
	w, body = c.json(http.MethodGet, "/api/vault/instances", nil)
	if w.Code != http.StatusOK || body["last_sync"] == nil {
		t.Errorf("after a pull, last_sync is %v", body["last_sync"])
	}
}

// A listing says nothing about reach on a vault that runs on one machine; the
// field appears only when a file is out of this machine's reach.
func TestListingCarriesReachOnlyWhenAFileIsOutOfIt(t *testing.T) {
	c := newTestClient(t)
	c.setup("a password", 3)
	c.upload("whole.txt", "/", []byte("all three parts here"))

	w, body := c.json(http.MethodGet, "/api/files?path=/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if _, present := body["unreachable"]; present {
		t.Errorf("a listing on one machine carries unreachable: %v", body["unreachable"])
	}

	// The same vault file opened on another machine: every folder it holds
	// was connected on this one, so none of them is there.
	v := c.server.vault
	desk := v.Instance()
	v.SetInstance("pi")
	w, body = c.json(http.MethodGet, "/api/files?path=/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	unreachable, _ := body["unreachable"].(map[string]any)
	if len(unreachable) != 1 {
		t.Fatalf("unreachable: %v, want the one file", body["unreachable"])
	}
	for _, reach := range unreachable {
		r, _ := reach.(map[string]any)
		if r["here"] != 0.0 || r["needed"] != 2.0 {
			t.Errorf("reach %v, want 0 of 2 here", r)
		}
	}

	// And the accounts panel says where those two live rather than pinging.
	w, body = c.json(http.MethodGet, "/api/providers", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("providers: %d %s", w.Code, w.Body.String())
	}
	elsewhere := 0
	for _, p := range body["providers"].([]any) {
		if p.(map[string]any)["elsewhere"] == desk {
			elsewhere++
		}
	}
	if elsewhere != 3 {
		t.Errorf("%d accounts say they live on %s, want 3: %v", elsewhere, desk, body["providers"])
	}
}
