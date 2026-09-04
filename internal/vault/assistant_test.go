package vault

import "testing"

func TestAssistantSettingsStartEmptyAndPersist(t *testing.T) {
	v, _ := newTestVault(t, 2)

	if got := v.Assistant(); got.Configured() || got != (AssistantSettings{}) {
		t.Fatalf("a fresh vault has an assistant: %+v", got)
	}

	want := AssistantSettings{URL: "http://gaming-pc:11434/v1/", Model: " qwen3:14b ", APIKey: "tok"}
	if err := v.SetAssistant(want); err != nil {
		t.Fatalf("SetAssistant: %v", err)
	}
	got := v.Assistant()
	if got.URL != "http://gaming-pc:11434/v1" || got.Model != "qwen3:14b" || got.APIKey != "tok" {
		t.Errorf("stored %+v, want it trimmed", got)
	}
	if !got.Configured() {
		t.Error("stored settings do not count as configured")
	}

	// Survives a lock and an unlock, which is what "persisted" means here.
	v.Lock()
	if v.Assistant() != (AssistantSettings{}) {
		t.Error("a locked vault still answers with the settings")
	}
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if again := v.Assistant(); again != got {
		t.Errorf("after unlock %+v, want %+v", again, got)
	}
}

func TestClearingTheAssistantURLClearsAllOfIt(t *testing.T) {
	v, _ := newTestVault(t, 2)

	if err := v.SetAssistant(AssistantSettings{URL: "http://x/v1", Model: "m", APIKey: "k"}); err != nil {
		t.Fatalf("SetAssistant: %v", err)
	}
	if err := v.SetAssistant(AssistantSettings{URL: "  ", Model: "still-here", APIKey: "k"}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := v.Assistant(); got != (AssistantSettings{}) {
		t.Errorf("after clearing: %+v", got)
	}

	// A model without a server is not a configuration.
	if err := v.SetAssistant(AssistantSettings{URL: "http://x/v1"}); err != nil {
		t.Fatalf("SetAssistant: %v", err)
	}
	if v.Assistant().Configured() {
		t.Error("a URL with no model counts as configured")
	}
}

func TestAssistantSettingsNeedTheVaultOpen(t *testing.T) {
	v, _ := newTestVault(t, 2)
	v.Lock()
	if err := v.SetAssistant(AssistantSettings{URL: "http://x/v1", Model: "m"}); err != ErrLocked {
		t.Fatalf("err %v, want ErrLocked", err)
	}
}
