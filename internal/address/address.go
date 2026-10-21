// Package address handles email address validation and generation of
// human-like random local parts.
package address

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// Local part limits. The RFC 5321 maximum is 64 octets; we stay within it and
// additionally cap generated parts well below that for readability.
const (
	MaxLocalPartLen = 64
	MaxDomainLen    = 253
)

// Split divides an address into its local part and domain, both lowercased and
// trimmed. It returns an error if the address is not a single, well-formed
// "local@domain" pair.
func Split(addr string) (local, domain string, err error) {
	a := strings.ToLower(strings.TrimSpace(addr))
	if a == "" {
		return "", "", fmt.Errorf("address is empty")
	}
	at := strings.LastIndexByte(a, '@')
	if at < 0 {
		return "", "", fmt.Errorf("address %q is missing '@'", addr)
	}
	local, domain = a[:at], a[at+1:]
	if local == "" {
		return "", "", fmt.Errorf("address %q has an empty local part", addr)
	}
	if domain == "" {
		return "", "", fmt.Errorf("address %q has an empty domain", addr)
	}
	return local, domain, nil
}

// ValidateLocalPart reports whether local is an acceptable mailbox local part.
//
// Blinkmail is a public service: allowing the full RFC 5322 quoted-string
// grammar would invite injection and homograph confusion. We accept the
// conservative unquoted subset used by virtually every real mailbox.
func ValidateLocalPart(local string) error {
	switch {
	case local == "":
		return fmt.Errorf("local part is empty")
	case len(local) > MaxLocalPartLen:
		return fmt.Errorf("local part exceeds %d characters", MaxLocalPartLen)
	case strings.Contains(local, "@"):
		return fmt.Errorf("local part must not contain '@'")
	case strings.ContainsAny(local, " \t\r\n"):
		return fmt.Errorf("local part must not contain whitespace")
	}

	const allowedSpecials = "._-+"
	for i := range len(local) {
		ch := local[i]
		isAlnum := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
		if !isAlnum && !strings.ContainsRune(allowedSpecials, rune(ch)) {
			return fmt.Errorf("local part contains invalid character %q", string(ch))
		}
	}

	if local[0] == '.' || local[len(local)-1] == '.' {
		return fmt.Errorf("local part must not start or end with a dot")
	}
	if strings.Contains(local, "..") {
		return fmt.Errorf("local part must not contain consecutive dots")
	}
	return nil
}

// ValidateDomain reports whether domain is a syntactically valid hostname.
func ValidateDomain(domain string) error {
	d := strings.ToLower(strings.TrimSpace(domain))
	switch {
	case d == "":
		return fmt.Errorf("domain is empty")
	case len(d) > MaxDomainLen:
		return fmt.Errorf("domain exceeds %d characters", MaxDomainLen)
	}

	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return fmt.Errorf("domain %q must contain at least one dot", domain)
	}
	for _, label := range labels {
		switch {
		case label == "":
			return fmt.Errorf("domain %q has an empty label", domain)
		case len(label) > 63:
			return fmt.Errorf("domain %q has a label longer than 63 characters", domain)
		case label[0] == '-' || label[len(label)-1] == '-':
			return fmt.Errorf("domain %q has a label starting or ending with a hyphen", domain)
		}
		for i := range len(label) {
			ch := label[i]
			isAlnum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
			if !isAlnum && ch != '-' {
				return fmt.Errorf("domain %q contains invalid character %q", domain, string(ch))
			}
		}
	}
	return nil
}

// Validate reports whether addr is a well-formed "local@domain" address.
func Validate(addr string) error {
	local, domain, err := Split(addr)
	if err != nil {
		return err
	}
	if err := ValidateLocalPart(local); err != nil {
		return err
	}
	return ValidateDomain(domain)
}

// Join builds an address from validated components.
func Join(local, domain string) string {
	return local + "@" + domain
}

// NullReversePath is the SMTP empty envelope sender (RFC 5321 3.6.3), used for
// bounces and auto-replies. Blinkmail accepts it as a valid envelope sender.
const NullReversePath = "<>"

// ValidateEnvelopeFrom accepts either the SMTP null reverse-path or a valid
// address. The envelope sender is only displayed, never used for routing.
func ValidateEnvelopeFrom(from string) error {
	f := strings.TrimSpace(from)
	if f == NullReversePath || f == "" {
		return nil
	}
	return Validate(f)
}

// The word lists below supply the vocabulary used to build human-like random
// local parts.
var (
	// firstWords are opening word choices.
	firstWords = []string{
		// Nature
		"langit", "senja", "pagi", "malam", "kopi", "hujan", "bulan", "bintang", "angin", "awan",
		"nusa", "rasa", "jalan", "teman", "cerita", "warna", "nadi", "cahya", "putra", "putri",
		"bagas", "adit", "arya", "dimas", "fajar", "rizky", "galih", "bayu", "bima", "nanda",
		"mega", "surya", "gunung", "lembah", "samudra", "rimba", "danau", "pelangi", "gemericik", "embun",
		"pelita", "akhasa", "sahara", "karang", "rawa", "gurun", "pantai", "matahari", "rembulan", "galaksi",
		// Animals
		"elang", "macan", "rajawali", "garuda", "merak", "kancil", "naga", "kuda", "cendrawasih", "lumba",
		"harimau", "cicak", "jalak", "tekukur", "kenari", "merpati", "kumbang", "lebah", "kupu", "belalang",
		// Plants
		"melati", "mawar", "anggrek", "teratai", "cemara", "pinus", "jati", "rotan", "randu", "mangga",
		"durian", "rambutan", "nangka", "pisang", "nanas", "salak", "duku", "pepaya", "sirsak", "manggis",
		// People
		"ananta", "arjuna", "bisma", "dewa", "dewi", "galuh", "kresna", "sinta", "wibawa", "wisesa",
		"agung", "abimanyu", "cakra", "dananjaya", "prabu", "ratna", "sakti", "satria", "wira", "widya",
	}

	// secondWords are trailing word choices.
	secondWords = []string{
		// Colors
		"biru", "jingga", "ungu", "merah", "kuning", "hijau", "putih", "emas", "perak", "abuabu",
		"nila", "merona", "kelabu", "keemasan", "kebiruan", "lembayung", "merjan", "dadumuda", "coklat", "krem",
		// Traits
		"manis", "tenang", "indah", "muda", "asri", "jaya", "lucu", "syahdu", "harum", "utama",
		"cerdas", "cepat", "lembut", "hangat", "murni", "setia", "mulia", "ramah", "santun", "gembira",
		"perkasa", "ganas", "kencang", "sabar", "ikhlas", "tabah", "cermat", "cekatan", "lapang", "damai",
		// Nature
		"laut", "hutan", "cerah", "pagi", "malam", "kecil", "besar", "dedaunan", "samudra", "rimbun",
		"gunung", "lembah", "sawah", "kali", "pulau", "tebing", "gua", "padang", "rawa", "bukit",
		// Objects
		"aji", "wira", "ayu", "sari", "nugraha", "permata", "lestari", "mahesa", "karya", "puspa",
		"rasa", "bakti", "citra", "daya", "dharma", "gita", "jiwa", "karsa", "kencana", "loka",
		"mustika", "nirmala", "bentala", "gerhana", "kirana", "mahkota", "mentari", "nusantara", "purnama", "sejati",
	}
)

// Generator produces random local parts using cryptographic randomness.
//
// Guessable addresses would let anyone read a stranger's inbox on a public
// service, so the reference implementation's math/rand is deliberately not
// used here.
type Generator struct{}

// NewGenerator returns a Generator backed by crypto/rand.
func NewGenerator() Generator { return Generator{} }

// RandomLocalPart returns a human-like local part such as "kopisenja47".
// About 80% of results carry a two-digit suffix, matching the reference
// behaviour.
func (Generator) RandomLocalPart() string {
	var b strings.Builder
	b.WriteString(pick(firstWords))
	b.WriteString(pick(secondWords))
	if randomInt(100) < 80 {
		fmt.Fprintf(&b, "%02d", randomInt(90)+10)
	}
	return b.String()
}

// RandomSuffix returns n decimal digits for collision fallbacks. Unlike the
// reference implementation this is not derived from the wall clock, which would
// be predictable to anyone watching request timing.
func (Generator) RandomSuffix(n int) string {
	var b strings.Builder
	for range n {
		fmt.Fprintf(&b, "%d", randomInt(10))
	}
	return b.String()
}

func pick(words []string) string {
	return words[randomInt(len(words))]
}

// randomInt returns a uniformly distributed integer in [0, n).
func randomInt(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// crypto/rand.Reader never fails in practice; a failure here means the
		// platform has no secure entropy source, which must not silently fall
		// back to something guessable.
		panic(fmt.Sprintf("address: crypto/rand unavailable: %v", err))
	}
	return int(v.Int64())
}
