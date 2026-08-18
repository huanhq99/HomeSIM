package main

import "testing"

func TestQADBKeyVectors(t *testing.T) {
	for challenge, expected := range map[string]string{
		"42790187": "cQfD.paNjDkltja",
		"17115309": "uWwxCQMVOz9IcTW",
		"33000465": "dhbXHZ/9doGNS4T",
	} {
		actual, err := deriveQADBKey(challenge)
		if err != nil || actual != expected {
			t.Fatalf("deriveQADBKey(%q) = %q, %v", challenge, actual, err)
		}
	}
}
