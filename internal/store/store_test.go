package store_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pocikode/blinkmail/internal/store"
)

// openTemp opens a store backed by a temporary SQLite file that is removed when
// the test ends. A real file (not :memory:) is used so persistence across a
// close/reopen cycle can be tested.
func openTemp(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blinkmail.db")
	s, err := store.Open(path, store.EmbeddedMigrations())
	if err != nil {
		t.Fatalf("store.Open(%q) = %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestMigrationsAreRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blinkmail.db")

	// Apply migrations three times in a row by opening and closing.
	for range 3 {
		s, err := store.Open(path, store.EmbeddedMigrations())
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if err := s.Health(context.Background()); err != nil {
			t.Fatalf("health after migration: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	// Every table from the migrations must exist exactly once: the four from the
	// reference model, the two auth tables added by 002_auth and api_keys from
	// 003_api_keys.
	s, err := store.Open(path, store.EmbeddedMigrations())
	if err != nil {
		t.Fatalf("final open: %v", err)
	}
	defer s.Close()

	want := map[string]bool{
		"inboxes": true, "messages": true, "sessions": true, "session_inboxes": true,
		"settings": true, "auth_sessions": true, "api_keys": true,
	}
	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for table := range want {
		if !got[table] {
			t.Errorf("expected table %q to exist, got tables %v", table, got)
		}
	}

	// schema_migrations must have recorded exactly one row per migration file,
	// so a re-run cannot have applied anything twice. The expected count is read
	// from the embedded set rather than hardcoded, so adding a migration does not
	// silently weaken this check.
	entries, err := fs.ReadDir(store.EmbeddedMigrations(), ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	wantMigrations := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			wantMigrations++
		}
	}
	var n int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != wantMigrations {
		t.Errorf("schema_migrations rows = %d, want %d", n, wantMigrations)
	}
}

func TestForeignKeysAndWALAreEnabled(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	var mode string
	if err := s.DB().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	var fk int
	if err := s.DB().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	// A message referencing a missing inbox must be rejected, proving FKs are
	// enforced at runtime rather than merely declared.
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO messages (id, inbox_address, from_address) VALUES (?, ?, ?)`,
		"orphan", "nobody@example.com", "x@example.com")
	if err == nil {
		t.Fatal("inserting a message for a missing inbox succeeded; foreign keys not enforced")
	}
}

func TestPersistenceAcrossCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blinkmail.db")
	ctx := context.Background()

	s, err := store.Open(path, store.EmbeddedMigrations())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := s.CreateSession(ctx, sessionToken(1)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := s.CreateInbox(ctx, "keep@example.com"); err != nil {
		t.Fatalf("create inbox: %v", err)
	}
	if err := s.LinkInboxToSession(ctx, sessionToken(1), "keep@example.com"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := s.DeliverMessage(ctx, store.NewMessage{
		ID: "msg-persist", Inbox: "keep@example.com", From: "a@b.com",
		Subject: "Persisted", Body: "survives restart", DeliveryKey: "key-persist",
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen the same file and confirm everything is still there.
	s2, err := store.Open(path, store.EmbeddedMigrations())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	msgs, err := s2.Messages(ctx, "keep@example.com")
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Subject != "Persisted" {
		t.Fatalf("messages after reopen = %+v, want the persisted row", msgs)
	}

	linked, err := s2.IsInboxInSession(ctx, sessionToken(1), "keep@example.com")
	if err != nil {
		t.Fatalf("is linked: %v", err)
	}
	if !linked {
		t.Error("session link did not persist across reopen")
	}

	inboxes, err := s2.SessionInboxes(ctx, sessionToken(1))
	if err != nil {
		t.Fatalf("session inboxes: %v", err)
	}
	if len(inboxes) != 1 {
		t.Errorf("session inboxes = %d, want 1", len(inboxes))
	}
}

func TestUnlinkPreservesMessagesAndAllowsReclaim(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	const addr = "shared@example.com"
	if err := s.CreateSession(ctx, sessionToken(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, sessionToken(2)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInbox(ctx, addr); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkInboxToSession(ctx, sessionToken(1), addr); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverMessage(ctx, store.NewMessage{
		ID: "m1", Inbox: addr, From: "x@y.com", Subject: "History", Body: "body",
		DeliveryKey: "k1",
	}); err != nil {
		t.Fatal(err)
	}

	// Session 1 removes the address from its own list.
	if err := s.UnlinkInboxFromSession(ctx, sessionToken(1), addr); err != nil {
		t.Fatalf("unlink: %v", err)
	}

	list, err := s.SessionInboxes(ctx, sessionToken(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("session 1 still lists %d inboxes after unlink", len(list))
	}

	// The inbox itself and its messages must survive.
	exists, err := s.InboxExists(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("inbox was deleted by unlink; it must be preserved")
	}
	msgs, err := s.Messages(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages after unlink = %d, want 1 (preserved)", len(msgs))
	}

	// Another session can claim the same address and read the history.
	if err := s.LinkInboxToSession(ctx, sessionToken(2), addr); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	linked, err := s.IsInboxInSession(ctx, sessionToken(2), addr)
	if err != nil {
		t.Fatal(err)
	}
	if !linked {
		t.Error("second session could not claim the address")
	}
	msgs2, err := s.Messages(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 1 || msgs2[0].Subject != "History" {
		t.Errorf("reclaimed history = %+v, want the preserved message", msgs2)
	}
}

func TestMailBeforeClaimIsReadableAfterClaim(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	const addr = "early@example.com"

	// Deliver mail to an address no browser has claimed yet. DeliverMessage
	// must create the inbox itself.
	if err := s.DeliverMessage(ctx, store.NewMessage{
		ID: "early-1", Inbox: addr, From: "sender@example.org",
		Subject: "Arrived first", Body: "before any claim", DeliveryKey: "early-key",
	}); err != nil {
		t.Fatalf("deliver to unclaimed inbox: %v", err)
	}

	exists, err := s.InboxExists(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("delivery did not auto-create the inbox")
	}

	// Now a browser claims it and must see the pre-existing message.
	if err := s.CreateSession(ctx, sessionToken(3)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInbox(ctx, addr); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.LinkInboxToSession(ctx, sessionToken(3), addr); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.Messages(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Subject != "Arrived first" {
		t.Errorf("messages after claim = %+v, want the message that arrived first", msgs)
	}
}

func TestDeliveryDeduplication(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	const addr = "dupe@example.com"
	key := store.DeliveryHash("from@example.org", addr, []byte("raw bytes"))

	msg := store.NewMessage{
		ID: "first", Inbox: addr, From: "from@example.org",
		Subject: "Once", Body: "body", DeliveryKey: key,
	}
	if err := s.DeliverMessage(ctx, msg); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	// The identical delivery must be reported as a duplicate, not stored twice.
	repeat := msg
	repeat.ID = "second"
	err := s.DeliverMessage(ctx, repeat)
	if !errors.Is(err, store.ErrDuplicateDelivery) {
		t.Fatalf("repeat delivery err = %v, want ErrDuplicateDelivery", err)
	}

	msgs, err := s.Messages(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("stored %d messages for a duplicate delivery, want 1", len(msgs))
	}
	if msgs[0].ID != "first" {
		t.Errorf("duplicate overwrote the original: id = %q, want first", msgs[0].ID)
	}

	// Different raw bytes for the same addresses must still be stored.
	other := msg
	other.ID = "third"
	other.DeliveryKey = store.DeliveryHash("from@example.org", addr, []byte("different bytes"))
	if err := s.DeliverMessage(ctx, other); err != nil {
		t.Fatalf("distinct delivery rejected: %v", err)
	}
	if msgs, err := s.Messages(ctx, addr); err != nil {
		t.Fatal(err)
	} else if len(msgs) != 2 {
		t.Errorf("messages = %d, want 2 after a genuinely different delivery", len(msgs))
	}
}

func TestMessageBodyHTMLRoundTrips(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	const addr = "html@example.com"
	msg := store.NewMessage{
		ID: "html-1", Inbox: addr, From: "from@example.org",
		Subject: "Rich", Body: "plain version",
		BodyHTML:    "<p>rich <b>version</b></p>",
		DeliveryKey: store.DeliveryHash("from@example.org", addr, []byte("html raw")),
	}
	if err := s.DeliverMessage(ctx, msg); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Every read path must return the HTML part, or the reader would silently
	// fall back to plain text.
	if got, err := s.Messages(ctx, addr); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 || got[0].BodyHTML != msg.BodyHTML {
		t.Fatalf("Messages BodyHTML = %q, want %q", got[0].BodyHTML, msg.BodyHTML)
	}
	if got, err := s.GetMessage(ctx, "html-1"); err != nil {
		t.Fatal(err)
	} else if got.BodyHTML != msg.BodyHTML {
		t.Errorf("GetMessage BodyHTML = %q, want %q", got.BodyHTML, msg.BodyHTML)
	}
	if got, err := s.MessagesSince(ctx, addr, "", 10); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 || got[0].BodyHTML != msg.BodyHTML {
		t.Errorf("MessagesSince BodyHTML = %q, want %q", got[0].BodyHTML, msg.BodyHTML)
	}

	// A message without an HTML part reads back as empty, not as a NULL scan
	// error, because the column is NOT NULL DEFAULT ''.
	plain := store.NewMessage{
		ID: "plain-1", Inbox: addr, From: "from@example.org",
		Subject: "Plain", Body: "only text",
		DeliveryKey: store.DeliveryHash("from@example.org", addr, []byte("plain raw")),
	}
	if err := s.DeliverMessage(ctx, plain); err != nil {
		t.Fatalf("deliver plain: %v", err)
	}
	got, err := s.GetMessage(ctx, "plain-1")
	if err != nil {
		t.Fatalf("get plain: %v", err)
	}
	if got.BodyHTML != "" {
		t.Errorf("plain message BodyHTML = %q, want empty", got.BodyHTML)
	}
}

// TestBackfillMigrationRepairsLegacyHTMLBodies covers 006_backfill_body_html.
//
// Before 005_message_html existed, an HTML-only message kept its markup in
// `body`, so after the column was added those rows read back with an empty
// `body_html` and the reader rendered raw markup. The migration copies `body`
// into `body_html`, but only for bodies that are recognisably HTML source.
func TestBackfillMigrationRepairsLegacyHTMLBodies(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	const addr = "legacy@example.com"
	if err := s.CreateInbox(ctx, addr); err != nil {
		t.Fatalf("create inbox: %v", err)
	}

	// Three legacy-shaped rows written with raw SQL so `body_html` keeps its
	// column default, exactly as rows stored before 005 look.
	const (
		htmlBody  = "<html><body><p>hi</p></body></html>"
		plainBody = "hello\nworld"
		prefilled = "<p>already</p>"
	)
	rows := []struct{ id, body, bodyHTML string }{
		{"legacy-html", htmlBody, ""},
		{"legacy-plain", plainBody, ""},
		{"legacy-set", "<p>old</p>", prefilled},
	}
	for _, r := range rows {
		if r.bodyHTML == "" {
			if _, err := s.DB().ExecContext(ctx,
				`INSERT INTO messages (id, inbox_address, from_address, body) VALUES (?, ?, ?, ?)`,
				r.id, addr, "from@example.org", r.body); err != nil {
				t.Fatalf("insert %s: %v", r.id, err)
			}
			continue
		}
		if _, err := s.DB().ExecContext(ctx,
			`INSERT INTO messages (id, inbox_address, from_address, body, body_html) VALUES (?, ?, ?, ?, ?)`,
			r.id, addr, "from@example.org", r.body, r.bodyHTML); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	stmt, err := fs.ReadFile(store.EmbeddedMigrations(), "006_backfill_body_html.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	// The file holds exactly one statement, so one Exec applies it whole.
	if _, err := s.DB().ExecContext(ctx, string(stmt)); err != nil {
		t.Fatalf("exec migration: %v", err)
	}

	bodyHTML := func(id string) string {
		t.Helper()
		var got string
		if err := s.DB().QueryRowContext(ctx,
			`SELECT body_html FROM messages WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("select body_html for %s: %v", id, err)
		}
		return got
	}

	if got := bodyHTML("legacy-html"); got != htmlBody {
		t.Errorf("HTML row body_html = %q, want the body %q", got, htmlBody)
	}
	if got := bodyHTML("legacy-plain"); got != "" {
		t.Errorf("plain row body_html = %q, want empty (must not be treated as HTML)", got)
	}
	if got := bodyHTML("legacy-set"); got != prefilled {
		t.Errorf("pre-filled row body_html = %q, want %q", got, prefilled)
	}

	// Running the migration again must change nothing.
	if _, err := s.DB().ExecContext(ctx, string(stmt)); err != nil {
		t.Fatalf("re-exec migration: %v", err)
	}
	if got := bodyHTML("legacy-html"); got != htmlBody {
		t.Errorf("after re-run HTML row body_html = %q, want %q", got, htmlBody)
	}
	if got := bodyHTML("legacy-plain"); got != "" {
		t.Errorf("after re-run plain row body_html = %q, want empty", got)
	}
	if got := bodyHTML("legacy-set"); got != prefilled {
		t.Errorf("after re-run pre-filled row body_html = %q, want %q", got, prefilled)
	}
}

func TestDeliveryHashIsStableAndCollisionResistant(t *testing.T) {
	raw := []byte("Subject: hi\n\nbody")

	// Same inputs give the same key.
	if a, b := store.DeliveryHash("f@x.com", "t@y.com", raw), store.DeliveryHash("f@x.com", "t@y.com", raw); a != b {
		t.Errorf("hash not stable: %q != %q", a, b)
	}
	// Each component must affect the key.
	base := store.DeliveryHash("f@x.com", "t@y.com", raw)
	cases := map[string]string{
		"different from":     store.DeliveryHash("g@x.com", "t@y.com", raw),
		"different to":       store.DeliveryHash("f@x.com", "u@y.com", raw),
		"different body":     store.DeliveryHash("f@x.com", "t@y.com", []byte("other")),
		"boundary shift":     store.DeliveryHash("f@x.comt@y.com", "", raw),
		"empty from vs null": store.DeliveryHash("", "t@y.com", raw),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("%s produced the same hash as the base case", name)
		}
	}
	// The boundary-shift case is the important one: without length framing,
	// "f@x.com"+"t@y.com" and "f@x.comt@y.com"+"" would hash identically.
	if len(base) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(base))
	}
}

func TestClaimInboxRejectsExistingAddress(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	const addr = "taken@example.com"
	if err := s.ClaimInbox(ctx, addr); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	err := s.ClaimInbox(ctx, addr)
	if !errors.Is(err, store.ErrAddressTaken) {
		t.Fatalf("second claim err = %v, want ErrAddressTaken", err)
	}
}

// TestConcurrentRandomAddressCreation exercises the property the API relies on:
// the PRIMARY KEY makes concurrent claims mutually exclusive, so two callers
// racing for the same generated address cannot both end up owning it.
//
// To force real contention (rather than hoping random names collide), every
// goroutine draws candidates from one small shared pool. The invariant is that
// each address is claimed by exactly one goroutine, and the total number of
// successful claims equals the number of distinct addresses.
func TestConcurrentRandomAddressCreation(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	const (
		workers   = 16
		perWorker = 12
		poolSize  = 40 // fewer slots than workers*perWorker guarantees collisions
	)

	// A fixed pool of candidate addresses, mimicking generated local parts.
	pool := make([]string, poolSize)
	for i := range pool {
		pool[i] = fmt.Sprintf("cand%02d@example.com", i)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed []string
		taken   int // collisions observed (ErrAddressTaken)
		errs    []error
	)

	start := make(chan struct{})
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := range perWorker {
				// Deterministic spread across the pool so workers overlap.
				idx := (worker*perWorker + i) % poolSize
				addr := pool[idx]

				err := s.ClaimInbox(ctx, addr)
				mu.Lock()
				switch {
				case err == nil:
					claimed = append(claimed, addr)
				case errors.Is(err, store.ErrAddressTaken):
					taken++
				default:
					errs = append(errs, err)
				}
				mu.Unlock()
			}
		}(w)
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("unexpected errors during concurrent claims: %v", errs)
	}

	// Every distinct address in the pool is claimed exactly once, no more.
	if len(claimed) != poolSize {
		t.Errorf("claimed %d addresses, want exactly %d distinct ones", len(claimed), poolSize)
	}
	seen := make(map[string]int, len(claimed))
	for _, addr := range claimed {
		seen[addr]++
	}
	for addr, n := range seen {
		if n != 1 {
			t.Errorf("address %q claimed %d times; the unique constraint failed", addr, n)
		}
	}

	// The rest of the attempts must have collided, proving contention occurred.
	if wantTaken := workers*perWorker - poolSize; taken != wantTaken {
		t.Errorf("observed %d collisions, want %d", taken, wantTaken)
	}

	// The database must agree with what the goroutines believe they claimed.
	var dbCount int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inboxes`).Scan(&dbCount); err != nil {
		t.Fatalf("count inboxes: %v", err)
	}
	if dbCount != poolSize {
		t.Errorf("inboxes in database = %d, want %d", dbCount, poolSize)
	}
}

// TestConcurrentDeliveryOfIdenticalMessageStoresOnce verifies that when many
// deliveries of the same message race, exactly one commits and the rest are
// acknowledged as duplicates — the behaviour the Worker depends on so it never
// stores a message twice yet still reports success.
func TestConcurrentDeliveryOfIdenticalMessageStoresOnce(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	const addr = "race@example.com"
	key := store.DeliveryHash("from@example.org", addr, []byte("same raw"))

	const attempts = 20
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		dupes     int
		other     []error
	)

	start := make(chan struct{})
	for n := range attempts {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			// Same delivery key, different message IDs: only the key is unique.
			err := s.DeliverMessage(ctx, store.NewMessage{
				ID: fmt.Sprintf("race-%d", n), Inbox: addr, From: "from@example.org",
				Subject: "Raced", Body: "body", DeliveryKey: key,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, store.ErrDuplicateDelivery):
				dupes++
			default:
				other = append(other, err)
			}
		}(n)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if succeeded != 1 {
		t.Errorf("exactly one concurrent delivery should commit, got %d", succeeded)
	}
	if dupes != attempts-1 {
		t.Errorf("expected %d duplicate acknowledgements, got %d", attempts-1, dupes)
	}

	msgs, err := s.Messages(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("stored %d rows for %d identical concurrent deliveries, want 1", len(msgs), attempts)
	}
}

func TestDeliverMessageIsAtomicOnFailure(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	// Occupy a message ID so the insert inside DeliverMessage must fail.
	if err := s.CreateInbox(ctx, "occupied@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverMessage(ctx, store.NewMessage{
		ID: "clash", Inbox: "occupied@example.com", From: "a@b.com",
		Subject: "First", Body: "b", DeliveryKey: "clash-key",
	}); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	// A delivery to a NEW inbox reusing that message ID must fail on the
	// messages primary key, and the transaction must roll back the inbox
	// creation rather than leaving an empty inbox behind.
	const fresh = "fresh@example.com"
	err := s.DeliverMessage(ctx, store.NewMessage{
		ID: "clash", Inbox: fresh, From: "a@b.com",
		Subject: "Second", Body: "b", DeliveryKey: "fresh-key",
	})
	if err == nil {
		t.Fatal("delivery with a duplicate message ID unexpectedly succeeded")
	}
	if errors.Is(err, store.ErrDuplicateDelivery) {
		t.Errorf("a primary-key clash on messages.id must not be reported as ErrDuplicateDelivery: %v", err)
	}

	exists, err := s.InboxExists(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("failed delivery left a partially created inbox; the transaction is not atomic")
	}
}

func TestGetInboxNotFound(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	_, err := s.GetInbox(ctx, "missing@example.com")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetInbox err = %v, want ErrNotFound", err)
	}
}

func TestTimestampsAreRFC3339UTC(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	const addr = "time@example.com"
	if err := s.CreateInbox(ctx, addr); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverMessage(ctx, store.NewMessage{
		ID: "t1", Inbox: addr, From: "a@b.com", Subject: "s", Body: "b", DeliveryKey: "t1",
	}); err != nil {
		t.Fatal(err)
	}

	inbox, err := s.GetInbox(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}

	// The reference stored "YYYY-MM-DD HH:MM:SS" with no zone, which browsers
	// parse as local time. Blinkmail stores RFC 3339 UTC so they parse as UTC.
	for name, value := range map[string]string{
		"inbox.created_at":    inbox.CreatedAt,
		"message.received_at": msgs[0].ReceivedAt,
	} {
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			t.Errorf("%s = %q, not RFC 3339: %v", name, value, err)
		}
	}
}

func TestHealthReportsDatabaseState(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	if err := s.Health(ctx); err != nil {
		t.Errorf("healthy store reported error: %v", err)
	}

	n, err := s.CountMessages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("message count = %d, want 0", n)
	}

	// A closed database must fail the health check rather than panic.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Health(ctx); err == nil {
		t.Error("health check passed on a closed database")
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := store.Open("", store.EmbeddedMigrations()); err == nil {
		t.Error("store.Open(\"\") succeeded, want an error")
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "blinkmail.db")
	s, err := store.Open(path, store.EmbeddedMigrations())
	if err != nil {
		t.Fatalf("open with missing parent dirs: %v", err)
	}
	defer s.Close()

	if err := s.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

// TestNullDeliveryKeysDoNotCollide verifies that rows without a delivery key
// (stored as SQL NULL) do not trip the UNIQUE constraint against each other.
func TestNullDeliveryKeysDoNotCollide(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	const addr = "nullkey@example.com"
	for i := range 3 {
		err := s.DeliverMessage(ctx, store.NewMessage{
			ID: fmt.Sprintf("nk-%d", i), Inbox: addr, From: "a@b.com",
			Subject: fmt.Sprintf("s%d", i), Body: "b",
			DeliveryKey: "", // no dedupe key
		})
		if err != nil {
			t.Fatalf("delivery %d with an empty delivery key: %v", i, err)
		}
	}

	msgs, err := s.Messages(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Errorf("stored %d messages with NULL delivery keys, want 3", len(msgs))
	}
}

// TestInboxSummariesAndMessagesSince covers the two /api/v1 queries: empty
// inboxes are excluded from the summary, counts and last activity are right,
// and the since filter compares instants rather than strings.
func TestInboxSummariesAndMessagesSince(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	// An inbox with no mail must not be summarised.
	if err := s.CreateInbox(ctx, "empty@example.com"); err != nil {
		t.Fatalf("CreateInbox: %v", err)
	}

	deliver := func(id, addr, subject string, ts string) {
		t.Helper()
		if err := s.DeliverMessage(ctx, store.NewMessage{
			ID: id, Inbox: addr, From: "a@b.com", Subject: subject, Body: "b",
		}); err != nil {
			t.Fatalf("DeliverMessage %s: %v", id, err)
		}
		if _, err := s.DB().ExecContext(ctx,
			`UPDATE messages SET received_at = ? WHERE id = ?`, ts, id); err != nil {
			t.Fatalf("set received_at: %v", err)
		}
	}

	// Variable fractional digits are deliberate: a lexicographic since would
	// order "…:02Z" after "…:02.500Z" because '.' sorts before 'Z'.
	deliver("m1", "a@example.com", "one", "2026-01-01T00:00:01Z")
	deliver("m2", "b@example.com", "two", "2026-01-01T00:00:02.500Z")
	deliver("m3", "b@example.com", "three", "2026-01-01T00:00:03Z")

	summaries, err := s.InboxSummaries(ctx)
	if err != nil {
		t.Fatalf("InboxSummaries: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v, want 2 (empty inbox excluded)", summaries)
	}
	if summaries[0].Address != "b@example.com" || summaries[0].MessageCount != 2 {
		t.Errorf("first summary = %+v, want b with 2 messages", summaries[0])
	}
	if summaries[0].LastReceivedAt != "2026-01-01T00:00:03Z" {
		t.Errorf("last_received_at = %q", summaries[0].LastReceivedAt)
	}
	if summaries[1].MessageCount != 1 || summaries[1].LastReceivedAt != "2026-01-01T00:00:01Z" {
		t.Errorf("second summary = %+v", summaries[1])
	}

	all, err := s.MessagesSince(ctx, "b@example.com", "", 10)
	if err != nil {
		t.Fatalf("MessagesSince(all): %v", err)
	}
	if len(all) != 2 || all[0].Subject != "two" || all[1].Subject != "three" {
		t.Errorf("MessagesSince = %+v, want two then three (oldest first)", all)
	}

	since, err := s.MessagesSince(ctx, "b@example.com", "2026-01-01T00:00:02Z", 10)
	if err != nil {
		t.Fatalf("MessagesSince(since): %v", err)
	}
	if len(since) != 2 {
		t.Errorf("MessagesSince(02Z) = %d rows, want 2 (julianday, not string order)", len(since))
	}

	limited, err := s.MessagesSince(ctx, "b@example.com", "", 1)
	if err != nil {
		t.Fatalf("MessagesSince(limit): %v", err)
	}
	if len(limited) != 1 || limited[0].Subject != "two" {
		t.Errorf("MessagesSince(limit 1) = %+v, want only the oldest", limited)
	}

	empty, err := s.MessagesSince(ctx, "empty@example.com", "", 10)
	if err != nil {
		t.Fatalf("MessagesSince(empty): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("MessagesSince(empty) = %#v, want a non-nil empty slice", empty)
	}
}

// TestMessagesSinceOrdersWithinTheSameSecond guards the oldest-first guarantee
// when received_at has variable fractional digits: "…:48.500Z" is later than
// "…:48Z" but sorts before it lexicographically, so a raw string ORDER BY would
// return the pair backwards and a client paging with since would skip a message.
func TestMessagesSinceOrdersWithinTheSameSecond(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	const addr = "samesecond@example.com"
	// The older message deliberately has the lexicographically greater id, so a
	// fallback id ordering cannot accidentally produce the right answer.
	cases := []struct{ id, subject, ts string }{
		{"zz-older", "zero-millis", "2026-01-01T00:00:48Z"},
		{"aa-newer", "half-second", "2026-01-01T00:00:48.500Z"},
	}
	for _, c := range cases {
		if err := s.DeliverMessage(ctx, store.NewMessage{
			ID: c.id, Inbox: addr, From: "a@b.com", Subject: c.subject, Body: "b",
		}); err != nil {
			t.Fatalf("DeliverMessage %s: %v", c.id, err)
		}
		if _, err := s.DB().ExecContext(ctx,
			`UPDATE messages SET received_at = ? WHERE id = ?`, c.ts, c.id); err != nil {
			t.Fatalf("set received_at: %v", err)
		}
	}

	msgs, err := s.MessagesSince(ctx, addr, "", 10)
	if err != nil {
		t.Fatalf("MessagesSince: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Subject != "zero-millis" || msgs[1].Subject != "half-second" {
		t.Errorf("order = [%s %s], want [zero-millis half-second] (oldest first)",
			msgs[0].Subject, msgs[1].Subject)
	}
}

// helpers ---------------------------------------------------------------------

func sessionToken(n int) string {
	// A well-formed 64-char lowercase hex token for tests.
	return fmt.Sprintf("%064x", n)
}
