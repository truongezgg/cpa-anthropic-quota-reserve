package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPickAuthHardBlocksWhenAllCandidatesReserved(t *testing.T) {
	configurePickAuthTest(t, true)

	raw, errPick := pickAuth(schedulerRequestJSON(t, "limited"))
	if errPick != nil {
		t.Fatalf("pickAuth() error = %v", errPick)
	}
	var got envelope
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if got.OK || got.Error == nil {
		t.Fatalf("pickAuth() response = %#v, want quota error", got)
	}
	if got.Error.Code != "anthropic_quota_reserve_exhausted" || got.Error.HTTPStatus != 429 {
		t.Fatalf("pickAuth() error = %#v", got.Error)
	}
}

func TestPickAuthSoftModeDelegatesWhenAllCandidatesReserved(t *testing.T) {
	configurePickAuthTest(t, false)

	raw, errPick := pickAuth(schedulerRequestJSON(t, "limited"))
	if errPick != nil {
		t.Fatalf("pickAuth() error = %v", errPick)
	}
	var got envelope
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	var result pluginapi.SchedulerPickResponse
	if errUnmarshal := json.Unmarshal(got.Result, &result); errUnmarshal != nil {
		t.Fatalf("unmarshal result: %v", errUnmarshal)
	}
	if !got.OK || result.Handled {
		t.Fatalf("pickAuth() response = %#v, result = %#v", got, result)
	}
}

func TestPickAuthHardModeUsesUnconfiguredFallback(t *testing.T) {
	configurePickAuthTest(t, true)

	raw, errPick := pickAuth(schedulerRequestJSON(t, "limited", "fallback"))
	if errPick != nil {
		t.Fatalf("pickAuth() error = %v", errPick)
	}
	var got envelope
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	var result pluginapi.SchedulerPickResponse
	if errUnmarshal := json.Unmarshal(got.Result, &result); errUnmarshal != nil {
		t.Fatalf("unmarshal result: %v", errUnmarshal)
	}
	if !got.OK || !result.Handled || result.AuthID != "fallback" {
		t.Fatalf("pickAuth() response = %#v, result = %#v", got, result)
	}
}

func TestPickAuthContainsMatchBlocksMatchingAuthID(t *testing.T) {
	currentConfig.Store(pluginConfig{
		HardBlockWhenAllReserved: true,
		Accounts: []accountConfig{{
			AuthID:           "borrowed@example.com",
			Match:            "contains",
			Reserve5hPercent: 20,
			Reserve7dPercent: 20,
		}},
	})
	stateMu.Lock()
	state = map[string]*accountState{
		"claude-0000aaaa-borrowed@example.com.json": {Util5h: 0.8, Reset5h: 4102444800},
	}
	stateMu.Unlock()
	t.Cleanup(func() {
		stateMu.Lock()
		state = map[string]*accountState{}
		stateMu.Unlock()
	})

	raw, errPick := pickAuth(schedulerRequestJSON(t, "claude-0000aaaa-borrowed@example.com.json"))
	if errPick != nil {
		t.Fatalf("pickAuth() error = %v", errPick)
	}
	var got envelope
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if got.OK || got.Error == nil || got.Error.HTTPStatus != 429 {
		t.Fatalf("pickAuth() response = %#v, want quota error", got)
	}
}

func TestMatchingAccountDefaultsToExact(t *testing.T) {
	acct, ok := matchingAccount("claude-0000aaaa-borrowed@example.com.json", []accountConfig{{AuthID: "borrowed@example.com"}})
	if ok || acct.AuthID != "" {
		t.Fatalf("matchingAccount() = %#v, %v; default match must be exact", acct, ok)
	}
}

func TestPickAuthContainsMatchFallsThroughToLowerTier(t *testing.T) {
	configurePickAuthTest(t, true)
	currentConfig.Store(pluginConfig{
		HardBlockWhenAllReserved: true,
		Accounts: []accountConfig{{
			AuthID:           "borrowed@example.com",
			Match:            "contains",
			Reserve5hPercent: 20,
			Reserve7dPercent: 20,
		}},
	})
	stateMu.Lock()
	state = map[string]*accountState{
		"claude-0000aaaa-borrowed@example.com.json": {Util5h: 0.8, Reset5h: 4102444800},
	}
	stateMu.Unlock()

	result := pickResult(t, tieredRequestJSON(t, "",
		testCandidate{"claude-0000aaaa-borrowed@example.com.json", 5},
		testCandidate{"claude-99cd37b2.json", 4},
		testCandidate{"claude-4c6c120c.json", 2},
	))
	if !result.Handled || result.AuthID != "claude-99cd37b2.json" {
		t.Fatalf("pickAuth() = %#v, want the next priority tier instead of a 429", result)
	}
}

// The host only offers a scheduler plugin the top priority tier unless it opts
// in here; losing this flag makes a reserved top-tier account 429 every request.
func TestRegistrationOptsIntoAcrossPriorities(t *testing.T) {
	caps := pluginRegistration().Capabilities
	if !caps.Scheduler || !caps.SchedulerAcrossPriorities || !caps.UsagePlugin {
		t.Fatalf("capabilities = %#v, want scheduler + scheduler_across_priorities + usage_plugin", caps)
	}
}

func configurePickAuthTest(t *testing.T, hardBlock bool) {
	t.Helper()
	currentConfig.Store(pluginConfig{
		HardBlockWhenAllReserved: hardBlock,
		Accounts: []accountConfig{{
			AuthID:           "limited",
			Reserve5hPercent: 20,
			Reserve7dPercent: 20,
		}},
	})
	stateMu.Lock()
	state = map[string]*accountState{
		"limited": {Util5h: 0.8, Reset5h: 4102444800},
	}
	stateMu.Unlock()
	resetSchedulerState()
	t.Cleanup(func() {
		stateMu.Lock()
		state = map[string]*accountState{}
		stateMu.Unlock()
		resetSchedulerState()
	})
}

func resetSchedulerState() {
	stickyMu.Lock()
	sticky = map[string]stickyEntry{}
	stickyMu.Unlock()
	cursorMu.Lock()
	cursors = map[string]int{}
	cursorMu.Unlock()
}

func schedulerRequestJSON(t *testing.T, authIDs ...string) []byte {
	t.Helper()
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(authIDs))
	for _, authID := range authIDs {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: authID, Provider: "claude"})
	}
	raw, errMarshal := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider:   "claude",
		Model:      "claude-sonnet",
		Candidates: candidates,
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	return raw
}

type testCandidate struct {
	id       string
	priority int
}

func tieredRequestJSON(t *testing.T, sessionID string, candidates ...testCandidate) []byte {
	t.Helper()
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, pluginapi.SchedulerAuthCandidate{ID: c.id, Provider: "claude", Priority: c.priority})
	}
	req := pluginapi.SchedulerPickRequest{
		Provider:   "claude",
		Model:      "claude-sonnet",
		Candidates: out,
	}
	if sessionID != "" {
		req.Options.Headers = map[string][]string{"X-Claude-Code-Session-Id": {sessionID}}
	}
	raw, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	return raw
}

func pickResult(t *testing.T, raw []byte) pluginapi.SchedulerPickResponse {
	t.Helper()
	out, errPick := pickAuth(raw)
	if errPick != nil {
		t.Fatalf("pickAuth() error = %v", errPick)
	}
	var got envelope
	if errUnmarshal := json.Unmarshal(out, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if !got.OK {
		t.Fatalf("pickAuth() response = %#v, want ok", got)
	}
	var result pluginapi.SchedulerPickResponse
	if errUnmarshal := json.Unmarshal(got.Result, &result); errUnmarshal != nil {
		t.Fatalf("unmarshal result: %v", errUnmarshal)
	}
	return result
}

func TestPickAuthDelegatesWhenNothingIsBlocked(t *testing.T) {
	configurePickAuthTest(t, true)
	stateMu.Lock()
	state["limited"].Util5h = 0.5
	stateMu.Unlock()

	result := pickResult(t, tieredRequestJSON(t, "s1", testCandidate{"limited", 5}, testCandidate{"fallback", 0}))
	if result.Handled {
		t.Fatalf("pickAuth() = %#v, want host to keep scheduling (and session affinity)", result)
	}
}

func TestPickAuthFallsThroughToLowerTierWhenTopTierReserved(t *testing.T) {
	configurePickAuthTest(t, true)

	result := pickResult(t, tieredRequestJSON(t, "", testCandidate{"limited", 5}, testCandidate{"fallback", 0}))
	if !result.Handled || result.AuthID != "fallback" {
		t.Fatalf("pickAuth() = %#v, want lower-tier fallback instead of a hard block", result)
	}
}

func TestPickAuthPrefersHighestPriorityValue(t *testing.T) {
	configurePickAuthTest(t, true)

	req := tieredRequestJSON(t, "", testCandidate{"limited", 9}, testCandidate{"low", 1}, testCandidate{"high", 3})
	for i := 0; i < 3; i++ {
		if result := pickResult(t, req); !result.Handled || result.AuthID != "high" {
			t.Fatalf("pick %d = %#v, want the eligible account with the largest priority", i, result)
		}
	}
}

func TestPickAuthKeepsSessionOnOneAccount(t *testing.T) {
	configurePickAuthTest(t, true)

	candidates := []testCandidate{{"limited", 5}, {"a", 0}, {"b", 0}}
	first := pickResult(t, tieredRequestJSON(t, "session-1", candidates...))
	for i := 0; i < 4; i++ {
		next := pickResult(t, tieredRequestJSON(t, "session-1", candidates...))
		if next.AuthID != first.AuthID {
			t.Fatalf("pick %d = %q, want sticky %q", i, next.AuthID, first.AuthID)
		}
	}
	other := pickResult(t, tieredRequestJSON(t, "session-2", candidates...))
	if other.AuthID == first.AuthID {
		t.Fatalf("session-2 = %q, want round-robin to spread new sessions", other.AuthID)
	}
}

func TestPickAuthRepicksWhenStickyAccountBecomesReserved(t *testing.T) {
	configurePickAuthTest(t, true)
	rememberSticky("claude|claude-sonnet|session-1", "limited")

	result := pickResult(t, tieredRequestJSON(t, "session-1", testCandidate{"limited", 5}, testCandidate{"fallback", 0}))
	if result.AuthID != "fallback" {
		t.Fatalf("pickAuth() = %#v, want a reserved sticky account to be replaced", result)
	}
}
