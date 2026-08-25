package security

import "testing"

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("valid password was rejected")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("invalid password was accepted")
	}
	if hash2, _ := HashPassword("correct horse battery staple"); hash == hash2 {
		t.Fatal("password hashes reused a salt")
	}
}

func TestPasswordMinimumLength(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password was accepted")
	}
	if _, err := HashPassword("密码密码密码密码"); err == nil {
		t.Fatal("four multibyte characters bypassed the character minimum")
	}
	if _, err := HashPassword("密码密码密码密码密码密码"); err != nil {
		t.Fatalf("twelve Unicode characters were rejected: %v", err)
	}
	if _, err := HashPassword(string(make([]byte, maxPasswordBytes+1))); err == nil {
		t.Fatal("oversized password was accepted")
	}
}

func TestTokenHashStableAndNoPlaintext(t *testing.T) {
	first := TokenHash("secret")
	if first != TokenHash("secret") {
		t.Fatal("token hash is not stable")
	}
	if first == "secret" || first == TokenHash("different") {
		t.Fatal("token hash is invalid")
	}
}
