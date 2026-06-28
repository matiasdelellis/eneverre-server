package auth

import "testing"

// Hashes produced by Werkzeug 3.0.4 generate_password_hash for "S3cret!pass".
const (
	wzScrypt = "scrypt:32768:8:1$yA7POSoMvVdAsX9c$c34f4aa4e8c7ec450a37c5c00aa8044dadb89e77ff2533ec5829dd1f23d1bb052c4d03be31a8f752e461d9141e2fe89ef28a003616f5d1ddaff27d5b552db646"
	wzPBKDF2 = "pbkdf2:sha256:600000$R6EWVV4aEfvGq6bE$8fdaeabf805366a0df47b33f849f0c61ea43704df022befb02463a495e07ccb8"
)

func TestCheckPasswordHash_Werkzeug(t *testing.T) {
	const pw = "S3cret!pass"
	for name, h := range map[string]string{"scrypt": wzScrypt, "pbkdf2": wzPBKDF2} {
		if !CheckPasswordHash(h, pw) {
			t.Errorf("%s: correct password rejected", name)
		}
		if CheckPasswordHash(h, "wrong") {
			t.Errorf("%s: wrong password accepted", name)
		}
	}
}

func TestCheckPasswordHash_Malformed(t *testing.T) {
	for _, h := range []string{"", "notahash", "md5$salt$x", "pbkdf2:sha256$salt$x"} {
		if CheckPasswordHash(h, "x") {
			t.Errorf("malformed hash %q accepted", h)
		}
	}
}

func TestGenerateRoundTrip(t *testing.T) {
	h := GeneratePasswordHash("hunter2")
	if !CheckPasswordHash(h, "hunter2") {
		t.Fatal("generated hash failed to verify")
	}
	if CheckPasswordHash(h, "hunter3") {
		t.Fatal("generated hash verified a wrong password")
	}
}
