package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pocikode/blinkmail/internal/api"
	"pocikode/blinkmail/internal/store"
)

// compile-time check: the concrete store satisfies the API's interface.
var _ api.Store = (*store.Store)(nil)

// errStorage is a synthetic storage failure injected by failingStore.
var errStorage = errors.New("injected storage failure")

// failingStore wraps a real *store.Store and fails specific operations, so the
// HTTP layer's error handling can be exercised without a broken database file.
type failingStore struct {
	inner          *store.Store
	failDeliver    bool
	failSession    bool
	failCreateInbx bool
	failClaim      bool
	failListInbox  bool
	failMessages   bool
	failLink       bool
	failUnlink     bool
	failIsInbox    bool
	failCount      bool
	failGetInbox   bool
}

func (f *failingStore) SessionExists(ctx context.Context, sid string) (bool, error) {
	if f.failSession {
		return false, errStorage
	}
	return f.inner.SessionExists(ctx, sid)
}
func (f *failingStore) CreateSession(ctx context.Context, sid string) error {
	if f.failSession {
		return errStorage
	}
	return f.inner.CreateSession(ctx, sid)
}
func (f *failingStore) CreateInbox(ctx context.Context, addr string) error {
	if f.failCreateInbx {
		return errStorage
	}
	return f.inner.CreateInbox(ctx, addr)
}
func (f *failingStore) ClaimInbox(ctx context.Context, addr string) error {
	if f.failClaim {
		return errStorage
	}
	return f.inner.ClaimInbox(ctx, addr)
}
func (f *failingStore) GetInbox(ctx context.Context, addr string) (store.Inbox, error) {
	if f.failGetInbox {
		return store.Inbox{}, errStorage
	}
	return f.inner.GetInbox(ctx, addr)
}
func (f *failingStore) SessionInboxes(ctx context.Context, sid string) ([]store.Inbox, error) {
	if f.failListInbox {
		return nil, errStorage
	}
	return f.inner.SessionInboxes(ctx, sid)
}
func (f *failingStore) LinkInboxToSession(ctx context.Context, sid, addr string) error {
	if f.failLink {
		return errStorage
	}
	return f.inner.LinkInboxToSession(ctx, sid, addr)
}
func (f *failingStore) UnlinkInboxFromSession(ctx context.Context, sid, addr string) error {
	if f.failUnlink {
		return errStorage
	}
	return f.inner.UnlinkInboxFromSession(ctx, sid, addr)
}
func (f *failingStore) IsInboxInSession(ctx context.Context, sid, addr string) (bool, error) {
	if f.failIsInbox {
		return false, errStorage
	}
	return f.inner.IsInboxInSession(ctx, sid, addr)
}
func (f *failingStore) Messages(ctx context.Context, addr string) ([]store.Message, error) {
	if f.failMessages {
		return nil, errStorage
	}
	return f.inner.Messages(ctx, addr)
}
func (f *failingStore) DeliverMessage(ctx context.Context, m store.NewMessage) error {
	if f.failDeliver {
		return errStorage
	}
	return f.inner.DeliverMessage(ctx, m)
}
func (f *failingStore) CountMessages(ctx context.Context) (int, error) {
	if f.failCount {
		return 0, errStorage
	}
	return f.inner.CountMessages(ctx)
}

// newFailingEnv builds a server whose store can be made to fail on demand.
func newFailingEnv(t *testing.T, mutate func(*failingStore)) (*api.Server, *failingStore, string) {
	t.Helper()
	e := newTestEnv(t)
	fs := &failingStore{inner: e.store}
	if mutate != nil {
		mutate(fs)
	}
	srv := api.New(api.Deps{
		Config: testConfig(e),
		Store:  fs,
		Logger: discardLogger(),
	})
	return srv, fs, e.secret
}

// deliver posts raw MIME to a given server.
func deliver(srv *api.Server, secret, raw, from, to string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/inbound", strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("X-Envelope-From", from)
	req.Header.Set("X-Envelope-To", to)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// TestInboundStorageFailureIsNotAcknowledged is the key delivery-integrity test:
// when storage fails, the endpoint must return a 5xx and must NOT report success.
// The reference implementation logged the failure and returned normally, which
// made Cloudflare acknowledge a message that was never stored.
func TestInboundStorageFailureIsNotAcknowledged(t *testing.T) {
	srv, _, secret := newFailingEnv(t, func(f *failingStore) {
		f.failDeliver = true
	})

	w := deliver(srv, secret, "Subject: Will fail\n\nbody", "from@x.com", "fail@mail.example.com")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on storage failure", w.Code)
	}
	// The response must not claim success.
	var resp struct {
		OK        bool   `json:"ok"`
		Duplicate bool   `json:"duplicate"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, w.Body.String())
	}
	if resp.OK || resp.Duplicate {
		t.Errorf("storage failure was acknowledged as success: %+v", resp)
	}
	if resp.Error == "" {
		t.Error("storage failure returned no error message")
	}
}

// TestInboundDuplicateIsAcknowledgedEvenWhenInnerStoreFailsOnOtherPaths ensures
// a genuine duplicate is still a 200. This complements the failure test above:
// only real errors become 5xx.
func TestInboundDuplicateStillSucceeds(t *testing.T) {
	e := newTestEnv(t)

	raw := "Subject: Twice\n\nbody"
	hdr := map[string]string{"Authorization": "Bearer " + e.secret}
	w := e.deliverRaw(raw, "from@x.com", "twice@mail.example.com", hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("first: %d", w.Code)
	}
	w = e.deliverRaw(raw, "from@x.com", "twice@mail.example.com", hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("second: %d", w.Code)
	}
	var resp struct {
		OK        bool `json:"ok"`
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.OK || !resp.Duplicate {
		t.Errorf("duplicate response = %+v, want ok+duplicate", resp)
	}
}

// TestSessionCreationStorageFailure verifies GET /api/session surfaces a 500
// when it cannot create a session, rather than returning a token that is not
// actually persisted.
func TestSessionCreationStorageFailure(t *testing.T) {
	srv, _, _ := newFailingEnv(t, func(f *failingStore) {
		f.failSession = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	// Must not hand out a session token on failure.
	if strings.Contains(w.Body.String(), "sessionId") {
		t.Errorf("body = %s, must not contain a sessionId on failure", w.Body.String())
	}
}

// TestListInboxesStorageFailure verifies a storage error becomes a 500.
func TestListInboxesStorageFailure(t *testing.T) {
	// Need a valid session first, then fail the listing.
	e := newTestEnv(t)
	sid := e.getSession(t)

	fs := &failingStore{inner: e.store, failListInbox: true}
	srv := api.New(api.Deps{Config: testConfig(e), Store: fs, Logger: discardLogger()})

	req := httptest.NewRequest(http.MethodGet, "/api/inboxes", nil)
	req.Header.Set("x-session-id", sid)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestMessagesStorageFailure verifies the messages route surfaces a 500.
func TestMessagesStorageFailure(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)
	addr, _ := e.createInbox(t, sid, "msgfail", "mail.example.com")

	fs := &failingStore{inner: e.store, failMessages: true}
	srv := api.New(api.Deps{Config: testConfig(e), Store: fs, Logger: discardLogger()})

	req := httptest.NewRequest(http.MethodGet, "/api/inboxes/"+urlEncode(addr)+"/messages", nil)
	req.Header.Set("x-session-id", sid)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestHealthReportsUnhealthyOnStorageFailure verifies /healthz returns 503 when
// the database cannot answer, which is what deployment checks rely on.
func TestHealthReportsUnhealthyOnStorageFailure(t *testing.T) {
	srv, _, _ := newFailingEnv(t, func(f *failingStore) {
		f.failCount = true
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != "unhealthy" {
		t.Errorf("status = %q, want unhealthy", resp.Status)
	}
}

// TestCreateInboxLinkFailure verifies a link failure after inbox creation is a
// 500 and does not return a 201 success.
func TestCreateInboxLinkFailure(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	fs := &failingStore{inner: e.store, failLink: true}
	srv := api.New(api.Deps{Config: testConfig(e), Store: fs, Logger: discardLogger()})

	req := httptest.NewRequest(http.MethodPost, "/api/inboxes",
		strings.NewReader(`{"localPart":"linkfail","domain":"mail.example.com"}`))
	req.Header.Set("x-session-id", sid)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}
