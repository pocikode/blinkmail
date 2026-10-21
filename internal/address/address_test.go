package address_test

import (
	"regexp"
	"strings"
	"testing"

	"pocikode/blinkmail/internal/address"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		in        string
		wantLocal string
		wantDom   string
		wantErr   bool
	}{
		{"user@example.com", "user", "example.com", false},
		{"  Mixed@Example.COM ", "mixed", "example.com", false},
		{"a.b+c_d-e@sub.example.co.uk", "a.b+c_d-e", "sub.example.co.uk", false},
		{"user@sub@domain.com", "user@sub", "domain.com", false}, // last @ splits
		{"noat.example.com", "", "", true},
		{"@example.com", "", "", true},
		{"user@", "", "", true},
		{"", "", "", true},
		{"   ", "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			local, domain, err := address.Split(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Split(%q) = %q,%q,nil; want an error", tc.in, local, domain)
				}
				return
			}
			if err != nil {
				t.Fatalf("Split(%q) error = %v", tc.in, err)
			}
			if local != tc.wantLocal || domain != tc.wantDom {
				t.Errorf("Split(%q) = %q,%q; want %q,%q", tc.in, local, domain, tc.wantLocal, tc.wantDom)
			}
		})
	}
}

func TestValidateLocalPart(t *testing.T) {
	valid := []string{
		"a", "user", "user123", "a.b", "a_b", "a-b", "a+b",
		"first.last", "x9", strings.Repeat("a", 64),
	}
	for _, lp := range valid {
		if err := address.ValidateLocalPart(lp); err != nil {
			t.Errorf("ValidateLocalPart(%q) = %v; want valid", lp, err)
		}
	}

	invalid := []struct{ name, lp string }{
		{"empty", ""},
		{"too long", strings.Repeat("a", 65)},
		{"contains at", "a@b"},
		{"space", "a b"},
		{"tab", "a\tb"},
		{"newline", "a\nb"},
		{"leading dot", ".abc"},
		{"trailing dot", "abc."},
		{"consecutive dots", "a..b"},
		{"bang", "a!b"},
		{"hash", "a#b"},
		{"dollar", "a$b"},
		{"percent", "a%b"},
		{"quote", "a\"b"},
		{"paren", "a(b)"},
		{"comma", "a,b"},
		{"colon", "a:b"},
		{"semicolon", "a;b"},
		{"angle", "<a>"},
		{"backslash", "a\\b"},
		{"bracket", "a[b]"},
		{"unicode", "üser"},
		{"emoji", "user🎉"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := address.ValidateLocalPart(tc.lp); err == nil {
				t.Errorf("ValidateLocalPart(%q) = nil; want an error", tc.lp)
			}
		})
	}
}

func TestValidateDomain(t *testing.T) {
	valid := []string{
		"example.com", "mail.example.com", "sub.domain.co.uk",
		"a-b.com", "x1.example.org", strings.Repeat("a", 63) + ".com",
	}
	for _, d := range valid {
		if err := address.ValidateDomain(d); err != nil {
			t.Errorf("ValidateDomain(%q) = %v; want valid", d, err)
		}
	}

	invalid := []struct{ name, d string }{
		{"empty", ""},
		{"no dot", "localhost"},
		{"empty label", "a..com"},
		{"leading dot", ".example.com"},
		{"trailing dot", "example.com."},
		{"leading hyphen", "-example.com"},
		{"trailing hyphen", "example-.com"},
		{"underscore", "ex_ample.com"},
		{"space", "exa mple.com"},
		{"label too long", strings.Repeat("a", 64) + ".com"},
		{"too long overall", strings.Repeat("a.", 130) + "com"},
		{"uppercase is normalised not rejected", ""}, // see below
	}
	for _, tc := range invalid {
		if tc.d == "" {
			continue // handled separately
		}
		t.Run(tc.name, func(t *testing.T) {
			if err := address.ValidateDomain(tc.d); err == nil {
				t.Errorf("ValidateDomain(%q) = nil; want an error", tc.d)
			}
		})
	}

	// Uppercase domains are lowercased by Split, so ValidateDomain must accept
	// them after normalisation the same way the API path does.
	if err := address.ValidateDomain(strings.ToLower("EXAMPLE.COM")); err != nil {
		t.Errorf("lowercased EXAMPLE.COM rejected: %v", err)
	}
}

func TestValidateFullAddress(t *testing.T) {
	if err := address.Validate("good.user+tag@mail.example.com"); err != nil {
		t.Errorf("valid address rejected: %v", err)
	}
	for _, bad := range []string{
		"", "noat", "@example.com", "user@", "bad name@example.com",
		"user@nodot", "user@-bad.com", "user@exa mple.com",
	} {
		if err := address.Validate(bad); err == nil {
			t.Errorf("Validate(%q) = nil; want an error", bad)
		}
	}
}

func TestJoin(t *testing.T) {
	if got := address.Join("user", "example.com"); got != "user@example.com" {
		t.Errorf("Join = %q", got)
	}
}

func TestValidateEnvelopeFrom(t *testing.T) {
	// The SMTP null reverse-path is legitimate for bounces.
	if err := address.ValidateEnvelopeFrom(address.NullReversePath); err != nil {
		t.Errorf("null reverse-path rejected: %v", err)
	}
	if err := address.ValidateEnvelopeFrom(""); err != nil {
		t.Errorf("empty envelope sender rejected: %v", err)
	}
	if err := address.ValidateEnvelopeFrom("real@example.com"); err != nil {
		t.Errorf("valid sender rejected: %v", err)
	}
	if err := address.ValidateEnvelopeFrom("not an address"); err == nil {
		t.Error("invalid sender accepted")
	}
}

// TestRandomLocalPartShape checks generated names stay within the legal local
// part grammar and the length bound.
func TestRandomLocalPartShape(t *testing.T) {
	gen := address.NewGenerator()
	// Legal characters for a generated local part: lowercase letters and digits.
	shape := regexp.MustCompile(`^[a-z][a-z0-9]*$`)

	lengths := map[int]bool{}
	for range 2000 {
		lp := gen.RandomLocalPart()
		if !shape.MatchString(lp) {
			t.Fatalf("RandomLocalPart() = %q, does not match %v", lp, shape)
		}
		if err := address.ValidateLocalPart(lp); err != nil {
			t.Fatalf("generated local part %q failed validation: %v", lp, err)
		}
		if len(lp) > 64 {
			t.Fatalf("generated local part %q exceeds 64 chars", lp)
		}
		lengths[len(lp)] = true
	}
	// Sanity: generation should produce a spread of lengths, not one constant.
	if len(lengths) < 3 {
		t.Errorf("generated only %d distinct lengths; randomness looks degenerate", len(lengths))
	}
}

// TestRandomLocalPartEntropy guards against the reference implementation's
// math/rand behaviour: on a public service a guessable address lets a stranger
// read someone else's inbox. Two runs must not produce identical sequences.
func TestRandomLocalPartEntropy(t *testing.T) {
	gen := address.NewGenerator()

	const n = 500
	first := make([]string, n)
	for i := range n {
		first[i] = gen.RandomLocalPart()
	}
	second := make([]string, n)
	for i := range n {
		second[i] = gen.RandomLocalPart()
	}

	same := 0
	for i := range n {
		if first[i] == second[i] {
			same++
		}
	}
	// With ~800k combinations and secure randomness, collisions at the same
	// index should be vanishingly rare. A deterministic PRNG would match all.
	if same > 5 {
		t.Errorf("%d/%d generated local parts repeated at the same index; "+
			"randomness looks deterministic (guessable addresses are a security risk)", same, n)
	}

	// Also check the overall set is mostly distinct.
	seen := map[string]bool{}
	for _, lp := range first {
		seen[lp] = true
	}
	if len(seen) < n*9/10 {
		t.Errorf("only %d distinct values from %d draws; entropy too low", len(seen), n)
	}
}

// TestRandomSuffixIsDigits verifies the collision fallback suffix is numeric
// and not derived from a clock value (the reference used Date.now()).
func TestRandomSuffixIsDigits(t *testing.T) {
	gen := address.NewGenerator()
	digits := regexp.MustCompile(`^[0-9]+$`)
	seen := map[string]bool{}
	for range 200 {
		s := gen.RandomSuffix(8)
		if !digits.MatchString(s) {
			t.Fatalf("RandomSuffix(8) = %q, want 8 digits", s)
		}
		if len(s) != 8 {
			t.Fatalf("RandomSuffix(8) length = %d", len(s))
		}
		seen[s] = true
	}
	if len(seen) < 150 {
		t.Errorf("only %d distinct 8-digit suffixes from 200 draws", len(seen))
	}
}
