package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// keyPermissions fetches GET /api/keys as the browser and returns one key's
// permissions as the documented JSON array.
func (e testEnv) keyPermissions(t *testing.T, id string) []string {
	t.Helper()
	w := e.doRequest(http.MethodGet, "/api/keys", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list keys: %d %s", w.Code, w.Body.String())
	}
	var keys []struct {
		ID          string   `json:"id"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil {
		t.Fatalf("list keys unmarshal: %v", err)
	}
	for _, k := range keys {
		if k.ID == id {
			return k.Permissions
		}
	}
	t.Fatalf("key %s not listed", id)
	return nil
}

// setReceivedAt rewrites a stored message's received_at so ordering and the
// since filter can be asserted without depending on how fast the test delivers.
func (e testEnv) setReceivedAt(t *testing.T, address, subject, ts string) {
	t.Helper()
	res, err := e.store.DB().ExecContext(t.Context(),
		`UPDATE messages SET received_at = ? WHERE inbox_address = ? AND subject = ?`,
		ts, address, subject)
	if err != nil {
		t.Fatalf("set received_at: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("set received_at: %d rows updated, want 1", n)
	}
}

// TestReadOnlyKeyCannotDeliver verifies the inbound gate: a read key can read
// but not inject mail.
func TestReadOnlyKeyCannotDeliver(t *testing.T) {
	e := newTestEnv(t)
	readKey, _ := e.createAPIKey(t, "reader", "read")

	w := e.deliverRaw("Subject: Nope\n\nbody", "from@x.com", "reader@mail.example.com",
		map[string]string{"Authorization": "Bearer " + readKey})
	if w.Code != http.StatusForbidden {
		t.Fatalf("inbound with read key: status = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "inbound") {
		t.Errorf("body = %s, want the missing permission named", w.Body.String())
	}

	w = e.doRequestRaw(http.MethodGet,
		"/api/v1/inboxes/reader%40mail.example.com/messages", nil,
		map[string]string{"Authorization": "Bearer " + readKey})
	if w.Code != http.StatusOK {
		t.Errorf("v1 read with read key: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestInboundOnlyKeyCannotRead verifies the other half: a relay key delivers
// mail but cannot read it, on either read surface.
func TestInboundOnlyKeyCannotRead(t *testing.T) {
	e := newTestEnv(t)
	relayKey, _ := e.createAPIKey(t, "relay-only", "inbound")

	w := e.deliverRaw("Subject: Delivered\n\nbody", "from@x.com", "relayonly@mail.example.com",
		map[string]string{"Authorization": "Bearer " + relayKey})
	if w.Code != http.StatusOK {
		t.Fatalf("inbound with inbound key: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	w = e.doRequestRaw(http.MethodGet,
		"/api/v1/inboxes/relayonly%40mail.example.com/messages", nil,
		map[string]string{"Authorization": "Bearer " + relayKey})
	if w.Code != http.StatusForbidden {
		t.Errorf("v1 read with inbound key: status = %d, want 403", w.Code)
	}

	// The legacy browser-shaped route is scoped too, so a key cannot sidestep
	// the v1 gate by using it instead.
	w = e.doRequestRaw(http.MethodGet,
		"/api/inboxes/relayonly%40mail.example.com/messages", nil,
		map[string]string{"Authorization": "Bearer " + relayKey})
	if w.Code != http.StatusForbidden {
		t.Errorf("legacy read with inbound key: status = %d, want 403", w.Code)
	}
}

// TestV1RejectsCookieCallers verifies /api/v1 is key-only: the UI must never
// depend on the integration contract.
func TestV1RejectsCookieCallers(t *testing.T) {
	e := newTestEnv(t)

	for _, path := range []string{
		"/api/v1/inboxes",
		"/api/v1/inboxes/x%40mail.example.com/messages",
	} {
		w := e.doRequest(http.MethodGet, path, nil, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("GET %s with cookie: status = %d, want 403", path, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), "API key required") {
			t.Errorf("GET %s body = %s, want API key required", path, w.Body.String())
		}
	}
}

// TestV1ReadsUnclaimedAddress is the point of the feature: deliver mail, then
// read it with a read key for an address no session ever claimed.
func TestV1ReadsUnclaimedAddress(t *testing.T) {
	e := newTestEnv(t)

	const addr = "neverclaimed@mail.example.com"
	w := e.deliverRaw("Subject: First\n\nhello", "from@x.com", addr,
		map[string]string{"Authorization": "Bearer " + e.apiKey})
	if w.Code != http.StatusOK {
		t.Fatalf("deliver: %d %s", w.Code, w.Body.String())
	}

	readKey, _ := e.createAPIKey(t, "integration", "read")
	w = e.doRequestRaw(http.MethodGet, "/api/v1/inboxes/"+urlEncode(addr)+"/messages", nil,
		map[string]string{"Authorization": "Bearer " + readKey})
	if w.Code != http.StatusOK {
		t.Fatalf("v1 messages: %d %s", w.Code, w.Body.String())
	}
	var msgs []struct {
		Subject      string `json:"subject"`
		InboxAddress string `json:"inbox_address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Subject != "First" || msgs[0].InboxAddress != addr {
		t.Errorf("messages = %+v, want the delivered message", msgs)
	}
}

// TestV1SinceAndOrdering verifies since filters by timestamp and results come
// oldest first, so a client paging with the last received_at never skips mail.
func TestV1SinceAndOrdering(t *testing.T) {
	e := newTestEnv(t)
	readKey, _ := e.createAPIKey(t, "integration", "read")

	const addr = "paged@mail.example.com"
	// Timestamps are set explicitly: deliveries inside one test can land in the
	// same millisecond, which would make the since boundary ambiguous.
	stamps := map[string]string{
		"One":   "2026-01-01T00:00:01Z",
		"Two":   "2026-01-01T00:00:02.500Z",
		"Three": "2026-01-01T00:00:03Z",
	}
	for _, subject := range []string{"One", "Two", "Three"} {
		w := e.deliverRaw("Subject: "+subject+"\n\nbody", "from@x.com", addr,
			map[string]string{"Authorization": "Bearer " + e.apiKey})
		if w.Code != http.StatusOK {
			t.Fatalf("deliver %s: %d", subject, w.Code)
		}
		e.setReceivedAt(t, addr, subject, stamps[subject])
	}

	get := func(query string) []string {
		t.Helper()
		w := e.doRequestRaw(http.MethodGet,
			"/api/v1/inboxes/"+urlEncode(addr)+"/messages"+query, nil,
			map[string]string{"Authorization": "Bearer " + readKey})
		if w.Code != http.StatusOK {
			t.Fatalf("messages%s: %d %s", query, w.Code, w.Body.String())
		}
		var msgs []struct {
			Subject string `json:"subject"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out := make([]string, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, m.Subject)
		}
		return out
	}

	if got := strings.Join(get(""), ","); got != "One,Two,Three" {
		t.Errorf("all messages = %s, want One,Two,Three (oldest first)", got)
	}

	// Paging from the first message's timestamp must return only the newer two.
	if got := strings.Join(get("?since="+url.QueryEscape(stamps["One"])), ","); got != "Two,Three" {
		t.Errorf("since %s = %s, want Two,Three", stamps["One"], got)
	}

	// The discriminating case: "…:02Z" is lexicographically *after*
	// "…:02.500Z" because '.' sorts before 'Z', so a string comparison would
	// drop "Two". julianday() compares instants and keeps it.
	if got := strings.Join(get("?since="+url.QueryEscape("2026-01-01T00:00:02Z")), ","); got != "Two,Three" {
		t.Errorf("since 2026-01-01T00:00:02Z = %s, want Two,Three", got)
	}

	// An invalid timestamp is a 400, not an empty page.
	w := e.doRequestRaw(http.MethodGet,
		"/api/v1/inboxes/"+urlEncode(addr)+"/messages?since=yesterday", nil,
		map[string]string{"Authorization": "Bearer " + readKey})
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid since: status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "RFC 3339") {
		t.Errorf("body = %s", w.Body.String())
	}
}

// TestV1Limit verifies the cap and the rejection of a non-positive limit.
func TestV1Limit(t *testing.T) {
	e := newTestEnv(t)
	readKey, _ := e.createAPIKey(t, "integration", "read")

	const addr = "limited@mail.example.com"
	for i := range 3 {
		subject := "M" + string(rune('0'+i))
		w := e.deliverRaw("Subject: "+subject+"\n\nbody", "from@x.com", addr,
			map[string]string{"Authorization": "Bearer " + e.apiKey})
		if w.Code != http.StatusOK {
			t.Fatalf("deliver %s: %d", subject, w.Code)
		}
		e.setReceivedAt(t, addr, subject, "2026-01-01T00:00:0"+string(rune('1'+i))+"Z")
	}

	w := e.doRequestRaw(http.MethodGet,
		"/api/v1/inboxes/"+urlEncode(addr)+"/messages?limit=2", nil,
		map[string]string{"Authorization": "Bearer " + readKey})
	if w.Code != http.StatusOK {
		t.Fatalf("limit=2: %d %s", w.Code, w.Body.String())
	}
	var msgs []struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("limit=2 returned %d messages, want 2", len(msgs))
	}

	for _, bad := range []string{"0", "-1", "many"} {
		w = e.doRequestRaw(http.MethodGet,
			"/api/v1/inboxes/"+urlEncode(addr)+"/messages?limit="+bad, nil,
			map[string]string{"Authorization": "Bearer " + readKey})
		if w.Code != http.StatusBadRequest {
			t.Errorf("limit=%s: status = %d, want 400", bad, w.Code)
		}
	}
}

// TestV1InboxListing verifies only inboxes with mail are listed, with a correct
// count, newest activity first.
func TestV1InboxListing(t *testing.T) {
	e := newTestEnv(t)
	readKey, _ := e.createAPIKey(t, "integration", "read")

	// An inbox with no messages must not appear.
	sid := e.getSession(t)
	if _, code := e.createInbox(t, sid, "emptybox", "mail.example.com"); code != http.StatusCreated {
		t.Fatalf("create empty inbox: %d", code)
	}

	deliveries := []struct {
		addr    string
		subject string
		stamp   string
	}{
		{"older@mail.example.com", "Old", "2026-01-01T00:00:01Z"},
		{"newer@mail.example.com", "New1", "2026-01-01T00:00:02Z"},
		{"newer@mail.example.com", "New2", "2026-01-01T00:00:03Z"},
	}
	for _, d := range deliveries {
		w := e.deliverRaw("Subject: "+d.subject+"\n\nbody", "from@x.com", d.addr,
			map[string]string{"Authorization": "Bearer " + e.apiKey})
		if w.Code != http.StatusOK {
			t.Fatalf("deliver to %s: %d", d.addr, w.Code)
		}
		e.setReceivedAt(t, d.addr, d.subject, d.stamp)
	}

	w := e.doRequestRaw(http.MethodGet, "/api/v1/inboxes", nil,
		map[string]string{"Authorization": "Bearer " + readKey})
	if w.Code != http.StatusOK {
		t.Fatalf("v1 inboxes: %d %s", w.Code, w.Body.String())
	}
	var summaries []struct {
		Address        string `json:"address"`
		CreatedAt      string `json:"created_at"`
		MessageCount   int    `json:"message_count"`
		LastReceivedAt string `json:"last_received_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &summaries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v, want only the two inboxes with mail", summaries)
	}
	if summaries[0].Address != "newer@mail.example.com" {
		t.Errorf("first = %q, want the most recently active inbox", summaries[0].Address)
	}
	if summaries[0].MessageCount != 2 {
		t.Errorf("newer message_count = %d, want 2", summaries[0].MessageCount)
	}
	if summaries[1].MessageCount != 1 {
		t.Errorf("older message_count = %d, want 1", summaries[1].MessageCount)
	}
	for _, s := range summaries {
		if s.Address == "emptybox@mail.example.com" {
			t.Error("an inbox with no messages was listed")
		}
		if s.CreatedAt == "" || s.LastReceivedAt == "" {
			t.Errorf("summary %+v has an empty timestamp", s)
		}
	}
}

// TestV1UnknownAddressIsEmptyArray verifies a fresh address is [] and not 404.
func TestV1UnknownAddressIsEmptyArray(t *testing.T) {
	e := newTestEnv(t)
	readKey, _ := e.createAPIKey(t, "integration", "read")

	w := e.doRequestRaw(http.MethodGet,
		"/api/v1/inboxes/nobody%40mail.example.com/messages", nil,
		map[string]string{"Authorization": "Bearer " + readKey})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("body = %s, want []", w.Body.String())
	}
}

// TestCreateAPIKeyPermissions covers the create-time rules: omitted means both,
// an empty array and an unknown name are 400, duplicates are canonicalised.
func TestCreateAPIKeyPermissions(t *testing.T) {
	e := newTestEnv(t)

	// Omitted: both, and the response echoes them.
	w := e.doRequest(http.MethodPost, "/api/keys", map[string]string{"name": "defaulted"},
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusCreated {
		t.Fatalf("default create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID          string   `json:"id"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if strings.Join(created.Permissions, ",") != "inbound,read" {
		t.Errorf("default permissions = %v, want inbound,read", created.Permissions)
	}

	// Explicitly empty: 400, not a default.
	w = e.doRequest(http.MethodPost, "/api/keys", map[string]interface{}{
		"name":        "empty",
		"permissions": []string{},
	}, map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty permissions: status = %d, want 400", w.Code)
	}

	// Unknown name: 400.
	w = e.doRequest(http.MethodPost, "/api/keys", map[string]interface{}{
		"name":        "bad",
		"permissions": []string{"write"},
	}, map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown permission: status = %d, want 400", w.Code)
	}

	// Duplicates collapse, and the stored order is canonical even when the
	// request lists read first.
	w = e.doRequest(http.MethodPost, "/api/keys", map[string]interface{}{
		"name":        "dupes",
		"permissions": []string{"read", "read", "inbound"},
	}, map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusCreated {
		t.Fatalf("duplicate permissions: %d %s", w.Code, w.Body.String())
	}
	var dupes struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &dupes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := e.keyPermissions(t, dupes.ID); strings.Join(got, ",") != "inbound,read" {
		t.Errorf("stored permissions = %v, want inbound,read", got)
	}

	// A single permission is stored alone, not padded out to both.
	_, readID := e.createAPIKey(t, "solo-read", "read")
	if got := e.keyPermissions(t, readID); strings.Join(got, ",") != "read" {
		t.Errorf("stored permissions = %v, want read", got)
	}
}

// TestListAPIKeysExposesPermissions verifies the array shape and that the hash
// still never appears.
func TestListAPIKeysExposesPermissions(t *testing.T) {
	e := newTestEnv(t)
	key, id := e.createAPIKey(t, "scoped", "read")

	body := e.doRequest(http.MethodGet, "/api/keys", nil, nil).Body.String()
	if strings.Contains(body, hashForTest(key)) {
		t.Error("list response contains the key hash")
	}
	if !strings.Contains(body, `"permissions":["read"]`) {
		t.Errorf("list body = %s, want a permissions array", body)
	}
	if got := e.keyPermissions(t, id); strings.Join(got, ",") != "read" {
		t.Errorf("permissions = %v, want [read]", got)
	}
}
