package identmask

import (
	"strings"
	"testing"
)

func TestEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"alice@example.com", "a***@e***.com"},
		{"A.Lice+tag@Mail.Example.co.uk", "A***@M***.uk"},
		{"é@ü.de", "é***@ü***.de"},
		{"x@localhost", "x***@l***"},
		{"  bob@example.org  ", "b***@e***.org"},
	}
	for _, c := range cases {
		got, err := Email(c.in)
		if err != nil || got != c.want {
			t.Errorf("Email(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// A value that is not clearly one email address is refused, never echoed
// and never turned into an empty or partial mask. PreviewLink treats the
// error as "could not observe" and refuses.
func TestEmailRefusesMalformed(t *testing.T) {
	for _, in := range []string{
		"", " ", "alice", "@example.com", "alice@", "a@b@c.com", "alice@.com",
		"alice@example.", "alice@exa mple.com", "al ice@example.com", "alice@exa\tmple.com",
		"a\x00@example.com", "alice@example..com",
		"alice@example.com,bob@example.com", "prn_abc123",
	} {
		got, err := Email(in)
		if err == nil || got != "" {
			t.Errorf("Email(%q) = %q, %v; want refusal", in, got, err)
		}
		if err != nil && strings.Contains(err.Error(), in) && in != "" && in != " " {
			t.Errorf("error for %q echoes its input: %v", in, err)
		}
	}
}

func TestEmailNeverRevealsMoreThanTheFirstRunes(t *testing.T) {
	got, err := Email("secretname@privatecorp.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"ecret", "rivate", "ame@"} {
		if strings.Contains(got, leak) {
			t.Errorf("mask %q leaks %q", got, leak)
		}
	}
}

var key = []byte("0123456789abcdef0123456789abcdef")

func TestPseudonymStableAndDistinct(t *testing.T) {
	a1, err := Pseudonym(key, "prn_alice")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := Pseudonym(key, "prn_alice")
	b, _ := Pseudonym(key, "prn_bob")
	if a1 != a2 {
		t.Errorf("not stable: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("distinct ids share a pseudonym: %q", a1)
	}
	if !strings.HasPrefix(a1, "pn_") || len(a1) != len("pn_")+16 {
		t.Errorf("unexpected shape %q", a1)
	}
	if strings.Contains(a1, "alice") {
		t.Errorf("pseudonym %q contains the id", a1)
	}
}

func TestPseudonymDependsOnKey(t *testing.T) {
	a, _ := Pseudonym(key, "prn_alice")
	other := append([]byte(nil), key...)
	other[0] ^= 1
	b, _ := Pseudonym(other, "prn_alice")
	if a == b {
		t.Error("a different key gave the same pseudonym")
	}
}

// No key, a short key or no id must be an error, never an unkeyed or
// constant pseudonym that would make every line look like the same user.
func TestPseudonymFailsClosed(t *testing.T) {
	if got, err := Pseudonym(nil, "prn_alice"); err == nil || got != "" {
		t.Errorf("nil key: %q, %v", got, err)
	}
	if got, err := Pseudonym(key[:15], "prn_alice"); err == nil || got != "" {
		t.Errorf("short key: %q, %v", got, err)
	}
	if got, err := Pseudonym(key, ""); err == nil || got != "" {
		t.Errorf("empty id: %q, %v", got, err)
	}
}

func TestPseudonymizerHidesKeyAndCopiesIt(t *testing.T) {
	k := append([]byte(nil), key...)
	p, err := NewPseudonymizer(k)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Pseudonym(key, "prn_alice")
	k[0] ^= 0xff // caller mutates its buffer afterwards
	if got := p.Of("prn_alice"); got != want {
		t.Errorf("Of = %q, want %q (key was not copied)", got, want)
	}
	if got := p.Of(""); got != Unknown {
		t.Errorf("empty id gave %q, want %q", got, Unknown)
	}
	if _, err := NewPseudonymizer(key[:8]); err == nil {
		t.Error("short key accepted")
	}
	var zero Pseudonymizer
	if got := zero.Of("prn_alice"); got != Unknown {
		t.Errorf("zero value gave %q, want %q", got, Unknown)
	}
	if s := p.String(); strings.Contains(s, "0123") {
		t.Errorf("String leaks key: %q", s)
	}
}
